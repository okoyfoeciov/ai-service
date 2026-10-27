package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AssemblyAI's Sync API: one multipart request, one finished transcript. The
// model is Universal-3.5 Pro; the X-AAI-Model header selects it. AssemblyAI
// accepts compressed comart recordings directly, so the batch route does not
// need ffmpeg. The live route in transcribe_stream.go does need it, because
// /v1/transcribe/live takes raw PCM.
var (
	assemblyAIURL     = "https://sync.assemblyai.com/v1/transcribe"
	assemblyAILiveURL = "https://sync.assemblyai.com/v1/transcribe/live"
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
