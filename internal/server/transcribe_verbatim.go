package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// handleTranscribeVerbatim takes one audio file from daily-dictation's import
// pipeline (multipart field "audio") and answers its transcript shape
// {"lines": [...], "words": [...]} from OpenRouter's MAI-Transcribe-2
// (verbose_json with word timestamps, verbatim style). This is the only route
// that still uses MAI-Transcribe-2; /api/transcribe runs on AssemblyAI.
func (s *Server) handleTranscribeVerbatim(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "multipart/form-data", func(mt string) bool {
		return mt == "multipart/form-data"
	}) {
		return
	}
	if s.cfg.OpenRouterTranscribeAPIKey == "" {
		jsonError(w, "server not configured: OPENROUTER_TRANSCRIBE_API_KEY missing", http.StatusInternalServerError)
		return
	}

	// OpenRouter caps Azure-backed MAI uploads at 25 MB. Leave room for the
	// multipart headers, then check the audio itself below.
	r.Body = http.MaxBytesReader(w, r.Body, openRouterSTTMaxBytes+2*1024*1024)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, "audio too long (over 25 MB)", http.StatusRequestEntityTooLarge)
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
	if len(audio) > openRouterSTTMaxBytes {
		jsonError(w, "audio too long (over 25 MB)", http.StatusRequestEntityTooLarge)
		return
	}
	filename := hdr.Filename
	if filename == "" {
		filename = "audio.mp3"
	}
	format, ok := openRouterAudioFormat(filename)
	if !ok {
		jsonError(w, "unsupported audio format (use WAV, MP3, FLAC, M4A, OGG, WebM or AAC)", http.StatusUnsupportedMediaType)
		return
	}

	body, err := newOpenRouterSTTBody(audio, format)
	if err != nil {
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), openRouterVerbatimTimeout)
	defer cancel()
	res, err := s.postOpenRouterSTT(ctx, body)
	if err != nil {
		jsonErrorDetail(w, "could not reach OpenRouter", http.StatusBadGateway, err.Error())
		return
	}
	raw := drainBody(res)

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// HTTP stays 502 (proxy failure); the body carries the real upstream
		// status. azureStatus is kept for daily-dictation's existing
		// permanent-vs-retryable classifier; openRouterStatus names the new
		// provider explicitly.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":            fmt.Sprintf("openrouter returned %d", res.StatusCode),
			"openRouterStatus": res.StatusCode,
			"azureStatus":      res.StatusCode,
			"detail":           string(raw),
		})
		return
	}

	var out openRouterSTTResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		jsonError(w, "openrouter returned unparseable JSON", http.StatusBadGateway)
		return
	}
	lines := out.lines()
	words := out.wordList()
	if len(lines) == 0 || len(words) == 0 {
		jsonError(w, "openrouter returned no transcript", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "words": words})
}
