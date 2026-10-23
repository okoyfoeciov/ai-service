package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"ai-service/internal/config"
)

var groqURL = "https://api.groq.com/openai/v1/audio/transcriptions"

const groqMaxBytes = 25 * 1024 * 1024 // per-clip ceiling for /api/transcribe

var (
	parakeetPollInterval = 100 * time.Millisecond
	parakeetPollTimeout  = 2 * time.Minute
)

// Azure's fast transcription endpoint is elastic: it answers 429 while it
// autoscales and 5xx for transient failures, and Microsoft's guidance is to
// retry those. Three attempts (the initial one plus two) with 1 s and 2 s
// backoff match /api/explain's policy. A Retry-After longer than 5 s is not
// worth blocking an interactive recording on.
const (
	azureMaxAttempts       = 3
	azureTranscribeTimeout = 3 * time.Minute
)

var azureRetryBase = time.Second // a variable so tests need not sleep

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

// azureRetryable reports the statuses Microsoft's fast transcription guidance
// says to retry: 429 while the service autoscales, and transient 5xx.
func azureRetryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func azureBackoff(attempt int) time.Duration {
	if azureRetryBase <= 0 {
		return 0
	}
	return time.Duration(1<<(attempt-1)) * azureRetryBase
}

// azureRetryWait is the delay before the next attempt: 1 s, then 2 s, or the
// provider's Retry-After when it gives one. A Retry-After over 5 s is not worth
// blocking an interactive recording on, so ok is false and the request fails.
func azureRetryWait(res *http.Response, attempt int) (d time.Duration, ok bool) {
	d = azureBackoff(attempt)
	if ra := strings.TrimSpace(res.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
			if d > 5*time.Second {
				return 0, false
			}
		}
	}
	return d, true
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// logTail bounds an upstream error body before it reaches the log.
func logTail(b []byte) string {
	const max = 600
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// handleTranscribe sends one clip from comart's mic recorder (Exscriptor) to
// the configured /api/transcribe provider (Groq Whisper, local Parakeet or
// Azure MAI-Transcribe-2) and answers {"text": ...}.
func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "an audio type", func(mt string) bool {
		return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/")
	}) {
		return
	}
	if s.cfg.TranscribeProvider == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "server not configured: none of GROQ_API_KEY, PARAKEET_URL, MAI_TRANSCRIBE_API_KEY is set",
		})
		return
	}
	contentType := r.Header.Get("Content-Type")

	r.Body = http.MaxBytesReader(w, r.Body, groqMaxBytes)
	audio, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "recording too long"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read body"})
		return
	}
	if len(audio) < 1200 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "recording too short"})
		return
	}

	ext := pickExt(contentType)

	switch s.cfg.TranscribeProvider {
	case config.ProviderGroq:
		s.transcribeGroq(w, r, audio, ext, contentType)
	case config.ProviderParakeet:
		s.transcribeParakeet(w, r, audio, ext, contentType)
	case config.ProviderMAITranscribe:
		s.transcribeMAI(w, r, audio, contentType)
	}
}

func (s *Server) transcribeGroq(w http.ResponseWriter, r *http.Request, audio []byte, ext, contentType string) {
	model := s.cfg.GroqModel
	if model == "" {
		model = "whisper-large-v3-turbo"
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename="audio.%s"`, ext)},
		"Content-Type":        {contentType},
	})
	_, _ = part.Write(audio)
	_ = form.WriteField("model", model)
	_ = form.WriteField("response_format", "json")
	_ = form.WriteField("temperature", "0")
	_ = form.Close()

	req, _ := http.NewRequestWithContext(r.Context(), "POST", groqURL, &body)
	req.Header.Set("Authorization", "Bearer "+s.cfg.GroqAPIKey)
	// Groq's API 403s requests with a missing or non-browser User-Agent. Set one
	// explicitly (Go sends its own otherwise).
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Content-Type", form.FormDataContentType())

	res, err := s.shortClient().Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not reach Groq", "detail": err.Error()})
		return
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		detail, _ := io.ReadAll(res.Body)
		var retryAfter any // null unless present
		if ra := res.Header.Get("Retry-After"); ra != "" {
			retryAfter = ra
		}
		// HTTP stays 502 (proxy failure); body carries the real Groq status.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      fmt.Sprintf("groq returned %d", res.StatusCode),
			"groqStatus": res.StatusCode,
			"retryAfter": retryAfter,
			"detail":     string(detail),
		})
		return
	}

	var data struct {
		Text string `json:"text"`
	}
	raw, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(raw, &data); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "groq returned unparseable JSON", "detail": string(raw)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": strings.TrimSpace(data.Text)})
}

// transcribeParakeet submits the clip to the local Parakeet service
// (~/parakeet-ov/service.py: POST /v1/jobs -> 202 {"job_id": ...}, then polls
// GET /v1/jobs/{job_id} until "done" or "error") and joins its lines into a
// single transcript string.
func (s *Server) transcribeParakeet(w http.ResponseWriter, r *http.Request, audio []byte, ext, contentType string) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename="audio.%s"`, ext)},
		"Content-Type":        {contentType},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	_, _ = part.Write(audio)
	_ = form.Close()

	baseURL := strings.TrimSuffix(s.cfg.ParakeetURL, "/")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, baseURL+"/v1/jobs", &body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	req.Header.Set("Content-Type", form.FormDataContentType())

	hc := s.shortClient()
	res, err := hc.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not reach Parakeet", "detail": err.Error()})
		return
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var retryAfter any
		if ra := res.Header.Get("Retry-After"); ra != "" {
			retryAfter = ra
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":          fmt.Sprintf("parakeet returned %d", res.StatusCode),
			"parakeetStatus": res.StatusCode,
			"retryAfter":     retryAfter,
			"detail":         string(raw),
		})
		return
	}

	var submitted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(raw, &submitted); err != nil || submitted.JobID == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "parakeet returned no job_id", "detail": string(raw)})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), parakeetPollTimeout)
	defer cancel()

	jobURL := baseURL + "/v1/jobs/" + url.PathEscape(submitted.JobID)
	for {
		timer := time.NewTimer(parakeetPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if r.Context().Err() != nil {
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "parakeet timed out"})
			return
		case <-timer.C:
		}

		pollReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, jobURL, nil)
		pollRes, err := hc.Do(pollReq)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not reach Parakeet", "detail": err.Error()})
			return
		}
		pollRaw, _ := io.ReadAll(pollRes.Body)
		pollRes.Body.Close()

		if pollRes.StatusCode < 200 || pollRes.StatusCode >= 300 {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":          fmt.Sprintf("parakeet returned %d", pollRes.StatusCode),
				"parakeetStatus": pollRes.StatusCode,
				"detail":         string(pollRaw),
			})
			return
		}

		var job struct {
			Status string  `json:"status"`
			Error  *string `json:"error"`
			Result *struct {
				Lines []struct {
					Text string `json:"text"`
				} `json:"lines"`
				Words []struct {
					Text string `json:"text"`
				} `json:"words"`
			} `json:"result"`
		}
		if err := json.Unmarshal(pollRaw, &job); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "parakeet returned unparseable JSON", "detail": string(pollRaw)})
			return
		}

		switch job.Status {
		case "queued", "processing":
			continue
		case "error":
			detail := ""
			if job.Error != nil {
				detail = *job.Error
			}
			msg := "parakeet job failed"
			if detail != "" {
				msg = "parakeet: " + detail
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": msg, "detail": detail})
			return
		case "done":
			var parts []string
			if job.Result != nil {
				for _, l := range job.Result.Lines {
					if t := strings.TrimSpace(l.Text); t != "" {
						parts = append(parts, t)
					}
				}
				if len(parts) == 0 {
					for _, wd := range job.Result.Words {
						if t := strings.TrimSpace(wd.Text); t != "" {
							parts = append(parts, t)
						}
					}
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"text": strings.Join(parts, " ")})
			return
		default:
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("parakeet returned unexpected status %q", job.Status)})
			return
		}
	}
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
	converted, err := prepareForAzure(ctx, audio)
	if err != nil {
		log.Printf("transcribe[MAI]: conversion failed (%d bytes, %q): %v", inputBytes, inputType, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "could not convert the recording for Azure",
			"detail": err.Error(),
		})
		return
	}
	audio, contentType = converted, "audio/wav"
	ext := "wav"

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="audio"; filename="audio.%s"`, ext)},
		"Content-Type":        {contentType},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	_, _ = part.Write(audio)
	_ = form.WriteField("definition", `{"locales":["en"],"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2"},"diarization":{"enabled":false}}`)
	_ = form.Close()
	bodyBytes := body.Bytes()

	endpoint := s.cfg.AzureSpeechEndpoint
	if endpoint == "" {
		endpoint = "https://eastus.api.cognitive.microsoft.com"
	}
	u := strings.TrimSuffix(endpoint, "/") +
		"/speechtotext/transcriptions:transcribe?api-version=" + azureAPIVersion

	var (
		status     int
		retryAfter string
		raw        []byte
		lastErr    error
	)
	for attempt := 1; attempt <= azureMaxAttempts; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
		if reqErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			return
		}
		req.Header.Set("Ocp-Apim-Subscription-Key", s.cfg.MAITranscribeAPIKey)
		req.Header.Set("Content-Type", form.FormDataContentType())

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
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not reach Azure", "detail": lastErr.Error()})
		return
	}
	if status < 200 || status >= 300 {
		var ra any
		if retryAfter != "" {
			ra = retryAfter
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":       fmt.Sprintf("azure returned %d", status),
			"azureStatus": status,
			"retryAfter":  ra,
			"detail":      string(raw),
		})
		return
	}

	var az struct {
		CombinedPhrases []struct {
			Text string `json:"text"`
		} `json:"combinedPhrases"`
		Phrases []azurePhrase `json:"phrases"`
	}
	if err := json.Unmarshal(raw, &az); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "azure returned unparseable JSON", "detail": string(raw)})
		return
	}

	phrases := append([]azurePhrase(nil), az.Phrases...)
	sort.Slice(phrases, func(i, j int) bool { return phrases[i].OffsetMs < phrases[j].OffsetMs })
	var parts []string
	for _, p := range phrases {
		if t := strings.TrimSpace(p.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		for _, cp := range az.CombinedPhrases {
			if t := strings.TrimSpace(cp.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": strings.Join(parts, " ")})
}
