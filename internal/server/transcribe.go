package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"ai-service/internal/config"
)

var groqURL = "https://api.groq.com/openai/v1/audio/transcriptions"

// maxTranscribeBytes is the per-clip ceiling for /api/transcribe. It is
// Groq's published limit and applies to every provider.
const maxTranscribeBytes = 25 * 1024 * 1024

// extByType maps a recording's MIME type to a file extension the STT backends
// recognise.
var extByType = [][2]string{
	{"mp4", "mp4"},
	{"m4a", "m4a"},
	{"mpeg", "mp3"},
	{"ogg", "ogg"},
	{"flac", "flac"},
	{"wav", "wav"},
	{"webm", "webm"},
}

func pickExt(contentType string) string {
	for _, pair := range extByType {
		if strings.Contains(contentType, pair[0]) {
			return pair[1]
		}
	}
	return "webm"
}

func (s *Server) shortClient() *http.Client {
	if s.httpShort != nil {
		return s.httpShort
	}
	return http.DefaultClient
}

// azureClient is the bounded client for Azure transcription. The MAI handler
// adds its own 3-minute context, so httpAzure's 10-minute timeout is only a
// backstop.
func (s *Server) azureClient() *http.Client {
	if s.httpAzure != nil {
		return s.httpAzure
	}
	return s.shortClient()
}

// headerOrNil is the header's value, or nil (JSON null) when it is absent.
func headerOrNil(h http.Header, name string) any { return nullableString(h.Get(name)) }

// nullableString is s, or nil (JSON null) when it is empty.
func nullableString(s string) any {
	if s != "" {
		return s
	}
	return nil
}

// TranscribeProviders returns the providers config.Load found, in the order
// the tray menu shows them. The slice is a copy.
func (s *Server) TranscribeProviders() []string {
	return append([]string(nil), s.transcribeProviders...)
}

// TranscribeProvider returns the provider /api/transcribe is using now.
func (s *Server) TranscribeProvider() string {
	if v, ok := s.transcribeProvider.Load().(string); ok && v != "" {
		return v
	}
	// A Server built by hand in a test has no atomic value; fall back to the
	// configuration it was given.
	return s.cfg.TranscribeProvider
}

// SetTranscribeProvider switches /api/transcribe to provider. It reports false
// for a provider that is not configured, so a stale saved selection or a bogus
// menu click cannot route a recording to a service without a key.
func (s *Server) SetTranscribeProvider(provider string) bool {
	for _, enabled := range s.transcribeProviders {
		if provider == enabled {
			s.transcribeProvider.Store(provider)
			return true
		}
	}
	return false
}

// handleTranscribe sends one clip from comart's mic recorder (Exscriptor) to
// the selected /api/transcribe provider (Groq Whisper or Azure
// MAI-Transcribe-2) and answers {"text": ...}.
func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "an audio type", func(mt string) bool {
		return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/")
	}) {
		return
	}
	provider := s.TranscribeProvider()
	if provider == "" {
		jsonError(w, "server not configured: neither GROQ_API_KEY nor MAI_TRANSCRIBE_API_KEY is set", http.StatusInternalServerError)
		return
	}
	contentType := r.Header.Get("Content-Type")

	r.Body = http.MaxBytesReader(w, r.Body, maxTranscribeBytes)
	audio, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, "recording too long", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "could not read body", http.StatusBadRequest)
		return
	}
	if len(audio) < 1200 {
		jsonError(w, "recording too short", http.StatusUnprocessableEntity)
		return
	}

	ext := pickExt(contentType)

	switch provider {
	case config.ProviderGroq:
		s.transcribeGroq(w, r, audio, ext, contentType)
	case config.ProviderMAITranscribe:
		s.transcribeMAI(w, r, audio, contentType)
	default:
		jsonError(w, "server not configured", http.StatusInternalServerError)
	}
}

func (s *Server) transcribeGroq(w http.ResponseWriter, r *http.Request, audio []byte, ext, contentType string) {
	model := s.cfg.GroqModel
	if model == "" {
		model = "whisper-large-v3-turbo"
	}

	body, formType, err := multipartBody("file", "audio."+ext, contentType, audio,
		[2]string{"model", model},
		[2]string{"response_format", "json"},
		[2]string{"temperature", "0"},
	)
	if err != nil {
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, groqURL, body)
	if err != nil {
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.GroqAPIKey)
	// Groq 403s requests with a non-browser User-Agent; Go sends its own by
	// default, so set a browser one explicitly.
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Content-Type", formType)

	res, err := s.shortClient().Do(req)
	if err != nil {
		jsonErrorDetail(w, "could not reach Groq", http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		detail, _ := io.ReadAll(res.Body)
		// HTTP stays 502 (proxy failure); body carries the real Groq status.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      fmt.Sprintf("groq returned %d", res.StatusCode),
			"groqStatus": res.StatusCode,
			"retryAfter": headerOrNil(res.Header, "Retry-After"),
			"detail":     string(detail),
		})
		return
	}

	var data struct {
		Text string `json:"text"`
	}
	raw, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(raw, &data); err != nil {
		jsonErrorDetail(w, "groq returned unparseable JSON", http.StatusBadGateway, string(raw))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": strings.TrimSpace(data.Text)})
}

// transcribeMAI sends the clip to Azure Speech Fast Transcription
// (MAI-Transcribe-2) using MAI_TRANSCRIBE_API_KEY and returns {"text": ...}.
// Separate from /api/transcribe-verbatim, which uses AZURE_SPEECH_KEY and
// returns word-level timestamps for daily-dictation.
func (s *Server) transcribeMAI(w http.ResponseWriter, r *http.Request, audio []byte, contentType string) {
	ctx, cancel := context.WithTimeout(r.Context(), azureTranscribeTimeout)
	defer cancel()

	// comart records audio/webm;codecs=opus, which Azure rejects with
	// invalid_audio. Decode every clip and send WAV (see audio.go).
	inputBytes, inputType := len(audio), contentType
	converted, err := convertForAzure(ctx, audio)
	if err != nil {
		log.Printf("transcribe[MAI]: conversion failed (%d bytes, %q): %v", inputBytes, inputType, err)
		jsonErrorDetail(w, "could not convert the recording for Azure", http.StatusBadGateway, err.Error())
		return
	}
	audio = converted

	endpoint := s.cfg.AzureSpeechEndpoint
	if endpoint == "" {
		endpoint = config.DefaultAzureSpeechEndpoint
	}
	upload, err := newAzureUpload(endpoint, "audio", "audio.wav", "audio/wav", audio, maiDefinition)
	if err != nil {
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}

	var (
		status     int
		retryAfter string
		raw        []byte
		lastErr    error
	)
	for attempt := 1; attempt <= azureMaxAttempts; attempt++ {
		req, reqErr := upload.request(ctx, s.cfg.MAITranscribeAPIKey)
		if reqErr != nil {
			jsonError(w, "internal", http.StatusInternalServerError)
			return
		}

		res, doErr := s.azureClient().Do(req)
		if doErr != nil {
			status, raw, lastErr = 0, nil, doErr
			log.Printf("transcribe[MAI]: input %d bytes %q, WAV %d bytes; attempt %d/%d: %v",
				inputBytes, inputType, len(audio), attempt, azureMaxAttempts, doErr)
			if attempt == azureMaxAttempts || ctx.Err() != nil || !sleepContext(ctx, azureBackoff(attempt)) {
				break
			}
			continue
		}
		raw, _ = io.ReadAll(res.Body)
		_ = res.Body.Close()
		status, retryAfter, lastErr = res.StatusCode, res.Header.Get("Retry-After"), nil
		if status >= 200 && status < 300 {
			break
		}
		log.Printf("transcribe[MAI]: input %d bytes %q, WAV %d bytes; attempt %d/%d: azure %d: %s",
			inputBytes, inputType, len(audio), attempt, azureMaxAttempts, status, logTail(raw))
		if attempt == azureMaxAttempts || !azureRetryable(status) {
			break
		}
		delay, ok := azureRetryWait(res, attempt)
		if !ok || !sleepContext(ctx, delay) {
			break
		}
	}
	if lastErr != nil {
		jsonErrorDetail(w, "could not reach Azure", http.StatusBadGateway, lastErr.Error())
		return
	}
	if status < 200 || status >= 300 {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":       fmt.Sprintf("azure returned %d", status),
			"azureStatus": status,
			"retryAfter":  nullableString(retryAfter),
			"detail":      string(raw),
		})
		return
	}

	var az azureResponse
	if err := json.Unmarshal(raw, &az); err != nil {
		jsonErrorDetail(w, "azure returned unparseable JSON", http.StatusBadGateway, string(raw))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": az.text()})
}
