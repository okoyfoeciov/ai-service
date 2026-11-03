package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ai-service/internal/prompt"
)

// handleAsk streams the answer to a spoken question about text the reader
// picked during the recording. It is deliberately not /api/explain: there is
// no vocabulary target and no Merriam-Webster work, only the question, the
// picked phrases and the surrounding passages.
//
// The SSE contract is /api/explain's: `chunk` events carry answer fragments,
// an optional `error` event carries a user-readable message, and `done` is
// always the last event. HTTP status stays 200 after the stream opens.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	provider := s.ExplainProvider()
	model := s.cfg.ExplainModelFor(provider)
	sw := &statusWriter{ResponseWriter: w}
	w = sw
	outcome := "ok"
	errMsg := ""
	items := 0
	contexts := 0
	defer func() {
		providerName, modelName := provider, model
		if providerName == "" {
			providerName = "none"
		}
		if modelName == "" {
			modelName = "-"
		}
		suffix := ""
		if errMsg != "" {
			errMsg = strings.ReplaceAll(errMsg, "\n", " ")
			if len(errMsg) > 240 {
				errMsg = errMsg[:240] + "…"
			}
			suffix = fmt.Sprintf(" error=%q", errMsg)
		}
		log.Printf("ask[%s]: model=%s status=%d outcome=%s items=%d contexts=%d duration=%s%s",
			providerName, modelName, sw.Status(), outcome, items, contexts,
			time.Since(start).Round(time.Millisecond), suffix)
	}()

	if !requireType(w, r, "application/json", isJSON) {
		outcome = "bad_content_type"
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body struct {
		Question string           `json:"question"`
		Items    []prompt.Item    `json:"items"`
		Contexts []prompt.Context `json:"contexts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			outcome = "too_large"
			jsonError(w, "Request too large.", http.StatusRequestEntityTooLarge)
			return
		}
		outcome = "invalid_json"
		jsonError(w, "Invalid JSON.", http.StatusBadRequest)
		return
	}
	items = len(body.Items)
	contexts = len(body.Contexts)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush() // open the stream now, not at the first event
	}
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	defer send("done", map[string]any{})

	ctx := r.Context()
	streamer := s.explainStreamer()
	if streamer == nil {
		outcome = "no_provider"
		errMsg = "Neither OPENROUTER_API_KEY nor CLAUDE_API_KEY is set."
		send("error", map[string]any{"message": errMsg})
		return
	}
	if strings.TrimSpace(body.Question) == "" {
		outcome = "no_question"
		errMsg = "No spoken question."
		send("error", map[string]any{"message": errMsg})
		return
	}

	p := prompt.BuildAsk(body.Question, body.Items, body.Contexts)
	result := streamer.Stream(ctx, p, func(t string) {
		send("chunk", map[string]any{"text": t})
	})
	if !result.OK {
		if ctx.Err() != nil {
			outcome = "client_gone"
			return
		}
		outcome = "error"
		errMsg = result.UserMessage()
		send("error", map[string]any{"message": errMsg})
	}
}
