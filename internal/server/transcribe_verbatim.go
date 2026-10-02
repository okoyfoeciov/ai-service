package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
)

const azureAPIVersion = "2025-10-15"

// azureMaxBytes is the Fast Transcription per-file ceiling (300 MB / ~2 h).
const azureMaxBytes = 300 * 1024 * 1024

// azureLine and azureWord are daily-dictation's transcript shape (seconds):
// lines carry the segment text, words carry per-word timings for the
// dictation loop's phrase looping.
type azureLine struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
}

type azureWord struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type azurePhraseWord struct {
	Text       string  `json:"text"`
	OffsetMs   float64 `json:"offsetMilliseconds"`
	DurationMs float64 `json:"durationMilliseconds"`
}

type azurePhrase struct {
	Text     string            `json:"text"`
	OffsetMs float64           `json:"offsetMilliseconds"`
	Words    []azurePhraseWord `json:"words"`
}

type azureResponse struct {
	Phrases []azurePhrase `json:"phrases"`
}

// handleTranscribeVerbatim takes one audio file from daily-dictation's import
// pipeline (multipart field "audio") and answers the repo's transcript shape
// {"lines": [...], "words": [...]} via Azure MAI-Transcribe-2 (verbatim with
// word timestamps). Diarization stays off: it shortens the supported length
// and dictation scores single-track verbatim references.
func (s *Server) handleTranscribeVerbatim(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "multipart/form-data", func(mt string) bool {
		return mt == "multipart/form-data"
	}) {
		return
	}
	if s.cfg.AzureSpeechKey == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "server not configured: AZURE_SPEECH_KEY missing"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, azureMaxBytes+1024*1024)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "audio too long (over 300 MB)"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read multipart body"})
		return
	}
	f, hdr, err := r.FormFile("audio")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "multipart field \"audio\" is required"})
		return
	}
	defer f.Close()
	audio, err := io.ReadAll(f)
	if err != nil || len(audio) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read audio"})
		return
	}
	filename := hdr.Filename
	if filename == "" {
		filename = "audio.mp3"
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("audio", filename)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	_, _ = part.Write(audio)
	_ = form.WriteField("definition", `{"locales":["en"],"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"word","transcribeStyle":"verbatim"}},"diarization":{"enabled":false}}`)
	_ = form.Close()

	url := strings.TrimSuffix(s.cfg.AzureSpeechEndpoint, "/") +
		"/speechtotext/transcriptions:transcribe?api-version=" + azureAPIVersion
	req, _ := http.NewRequestWithContext(r.Context(), "POST", url, &body)
	req.Header.Set("Ocp-Apim-Subscription-Key", s.cfg.AzureSpeechKey)
	req.Header.Set("Content-Type", form.FormDataContentType())

	res, err := s.httpAzure.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not reach Azure", "detail": err.Error()})
		return
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// HTTP stays 502 (proxy failure); body carries the real Azure status.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":       fmt.Sprintf("azure returned %d", res.StatusCode),
			"azureStatus": res.StatusCode,
			"detail":      string(raw),
		})
		return
	}

	var az azureResponse
	if err := json.Unmarshal(raw, &az); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "azure returned unparseable JSON"})
		return
	}

	phrases := append([]azurePhrase(nil), az.Phrases...)
	sort.Slice(phrases, func(i, j int) bool { return phrases[i].OffsetMs < phrases[j].OffsetMs })
	lines := make([]azureLine, 0, len(phrases))
	words := make([]azureWord, 0, 256)
	for _, p := range phrases {
		if t := strings.TrimSpace(p.Text); t != "" {
			lines = append(lines, azureLine{Text: t, Start: p.OffsetMs / 1000})
		}
		for _, wd := range p.Words {
			if t := strings.TrimSpace(wd.Text); t != "" {
				start := wd.OffsetMs / 1000
				words = append(words, azureWord{Text: t, Start: start, End: start + wd.DurationMs/1000})
			}
		}
	}
	if len(lines) == 0 || len(words) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "azure returned no transcript"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "words": words})
}
