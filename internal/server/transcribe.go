package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

const groqURL = "https://api.groq.com/openai/v1/audio/transcriptions"

const groqMaxBytes = 25 * 1024 * 1024 // Groq Whisper per-file ceiling

// extByType maps a recording's MIME type to a file extension Groq recognises.
var extByType = [][2]string{
	{"mp4", "mp4"},
	{"m4a", "m4a"},
	{"mpeg", "mp3"},
	{"ogg", "ogg"},
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

// handleTranscribe sends one clip from comart's mic recorder (Exscriptor) to
// Groq Whisper and answers {"text": ...}.
func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "an audio type", func(mt string) bool {
		return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/")
	}) {
		return
	}
	if s.cfg.GroqAPIKey == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "server not configured: GROQ_API_KEY missing"})
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

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename="audio.%s"`, ext)},
		"Content-Type":        {contentType},
	})
	_, _ = part.Write(audio)
	_ = form.WriteField("model", s.cfg.GroqModel)
	_ = form.WriteField("response_format", "json")
	_ = form.WriteField("temperature", "0")
	_ = form.Close()

	req, _ := http.NewRequestWithContext(r.Context(), "POST", groqURL, &body)
	req.Header.Set("Authorization", "Bearer "+s.cfg.GroqAPIKey)
	// Groq's API 403s requests with a missing or non-browser User-Agent. Set one
	// explicitly (Go sends its own otherwise).
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Content-Type", form.FormDataContentType())

	res, err := s.httpShort.Do(req)
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
	_ = json.Unmarshal(raw, &data)
	writeJSON(w, http.StatusOK, map[string]any{"text": strings.TrimSpace(data.Text)})
}
