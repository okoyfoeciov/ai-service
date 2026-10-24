package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// handleTranscribeVerbatim takes one audio file from daily-dictation's import
// pipeline (multipart field "audio") and answers its transcript shape
// {"lines": [...], "words": [...]} via Azure MAI-Transcribe-2 (verbatim with
// word timestamps).
func (s *Server) handleTranscribeVerbatim(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "multipart/form-data", func(mt string) bool {
		return mt == "multipart/form-data"
	}) {
		return
	}
	if s.cfg.AzureSpeechKey == "" {
		jsonError(w, "server not configured: AZURE_SPEECH_KEY missing", http.StatusInternalServerError)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, azureMaxBytes+1024*1024)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, "audio too long (over 300 MB)", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "could not read multipart body", http.StatusBadRequest)
		return
	}
	f, hdr, err := r.FormFile("audio")
	if err != nil {
		jsonError(w, `multipart field "audio" is required`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	audio, err := io.ReadAll(f)
	if err != nil || len(audio) == 0 {
		jsonError(w, "could not read audio", http.StatusBadRequest)
		return
	}
	filename := hdr.Filename
	if filename == "" {
		filename = "audio.mp3"
	}

	upload, err := newAzureUpload(s.cfg.AzureSpeechEndpoint, "audio", filename, "", audio, verbatimDefinition)
	if err != nil {
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}
	req, err := upload.request(r.Context(), s.cfg.AzureSpeechKey)
	if err != nil {
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}

	res, err := s.azureClient().Do(req)
	if err != nil {
		jsonErrorDetail(w, "could not reach Azure", http.StatusBadGateway, err.Error())
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
		jsonError(w, "azure returned unparseable JSON", http.StatusBadGateway)
		return
	}

	lines := make([]azureLine, 0, len(az.Phrases))
	words := make([]azureWord, 0, 256)
	for _, p := range az.sortedPhrases() {
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
		jsonError(w, "azure returned no transcript", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "words": words})
}
