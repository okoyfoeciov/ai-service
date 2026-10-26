package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"ai-service/internal/config"
)

// AssemblyAI's Sync API: one multipart request, one finished transcript. The
// model is Universal-3.5 Pro; the X-AAI-Model header selects it. Unlike the
// OpenRouter MAI path, AssemblyAI accepts compressed comart recordings
// directly, so this provider does not need ffmpeg.
//
// Potential improvement (not implemented): AssemblyAI's /v1/transcribe/live
// accepts audio while it is still being recorded, up to 120 seconds. Streaming
// chunks from comart would overlap upload and inference with the recording and
// remove most of the post-stop wait, but it needs a chunked client protocol.
var (
	assemblyAIURL     = "https://sync.assemblyai.com/v1/transcribe"
	assemblyAIWarmURL = "https://sync.assemblyai.com/v1/warm"
)

const (
	assemblyAIModel   = "universal-3-5-pro"
	assemblyAITimeout = 60 * time.Second // the API's own server deadline is 30 s
)

type assemblyAIResponse struct {
	Text string `json:"text"`
}

// transcribeAssemblyAI sends the clip to AssemblyAI's Sync API and returns
// {"text": ...}.
func (s *Server) transcribeAssemblyAI(w http.ResponseWriter, r *http.Request, audio []byte, ext string) {
	partType := assemblyAIPartType(ext)
	body, formType, err := multipartBody("audio", "audio."+ext, partType, audio)
	if err != nil {
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), assemblyAITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, assemblyAIURL, body)
	if err != nil {
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}
	// AssemblyAI documents the raw key here; a "Bearer " prefix is optional.
	req.Header.Set("Authorization", s.cfg.AssemblyAIKey)
	req.Header.Set("X-AAI-Model", assemblyAIModel)
	req.Header.Set("Content-Type", formType)

	res, err := s.shortClient().Do(req)
	if err != nil {
		jsonErrorDetail(w, "could not reach AssemblyAI", http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		detail, _ := io.ReadAll(res.Body)
		// HTTP stays 502 (proxy failure); body carries the real AssemblyAI
		// status and its RFC 9457 problem detail.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":            fmt.Sprintf("assemblyai returned %d", res.StatusCode),
			"assemblyAIStatus": res.StatusCode,
			"retryAfter":       headerOrNil(res.Header, "Retry-After"),
			"detail":           string(detail),
		})
		return
	}

	raw, _ := io.ReadAll(res.Body)
	var out assemblyAIResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		jsonErrorDetail(w, "assemblyai returned unparseable JSON", http.StatusBadGateway, string(raw))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": strings.TrimSpace(out.Text)})
}

// handleTranscribeWarm opens the AssemblyAI HTTPS connection while the client
// is still recording, so the eventual POST does not pay DNS/TCP/TLS setup. It
// is a no-op for every other provider: Groq and OpenRouter do not need it.
func (s *Server) handleTranscribeWarm(w http.ResponseWriter, r *http.Request) {
	if s.TranscribeProvider() != config.ProviderAssemblyAI {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.warmAssemblyAI(ctx); err != nil {
		// Warm-up is an optimisation; a failed one must not fail the recording.
		log.Printf("transcribe[warm]: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// warmAssemblyAI opens and keeps the connection in s.shortClient's pool. It
// uses the same client and host as transcribeAssemblyAI, which is what makes
// the later POST reuse it.
func (s *Server) warmAssemblyAI(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assemblyAIWarmURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-AAI-Model", assemblyAIModel)
	res, err := s.shortClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("assemblyai warm returned %d", res.StatusCode)
	}
	return nil
}

// assemblyAIPartType maps the extension chosen by pickExt to the Content-Type
// AssemblyAI expects on the audio part. M4A containers are audio/mp4.
func assemblyAIPartType(ext string) string {
	switch ext {
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "m4a", "mp4":
		return "audio/mp4"
	case "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "webm":
		return "audio/webm"
	case "aac":
		return "audio/aac"
	}
	return "application/octet-stream"
}
