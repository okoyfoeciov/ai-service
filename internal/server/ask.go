package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ai-service/internal/config"
	"ai-service/internal/llm"
	"ai-service/internal/prompt"
)

// AskProviders returns the configured ask providers, in the order the tray
// menu shows them. The slice is a copy.
func (s *Server) AskProviders() []string {
	return append([]string(nil), s.askProviders...)
}

// AskProvider returns the provider /api/ask is using now.
func (s *Server) AskProvider() string {
	if v, ok := s.askProvider.Load().(string); ok && v != "" {
		return v
	}
	// A Server built by hand in a test has no atomic value; fall back to the
	// configuration it was given.
	return s.cfg.AskProvider
}

// SetAskProvider switches /api/ask to provider. It reports false for a
// provider that is not configured, so a stale saved selection or a bogus menu
// click cannot route a question to a service without a key.
func (s *Server) SetAskProvider(provider string) bool {
	for _, enabled := range s.askProviders {
		if provider == enabled {
			s.askProvider.Store(provider)
			return true
		}
	}
	return false
}

// The Ask routes send every round's screenshots, so these caps keep one card
// inside Cerebras' per-request image/payload limits. The client also refuses
// to capture beyond maxAskImages per Ask card.
const (
	maxAskImages      = 10
	maxAskImageBase64 = 8 << 20
)

// handleAsk streams the answer to a spoken question about text the reader
// picked during the recording. It is deliberately not /api/explain: there is
// no vocabulary target and no Merriam-Webster work, only the question, the
// picked phrases and the surrounding passages. It also has its own provider
// selection, so an answer can run on a different model from an explanation.
//
// The SSE contract is /api/explain's: `chunk` events carry answer fragments,
// an optional `error` event carries a user-readable message, and `done` is
// always the last event. Every Ask starts with a `search` event
// ({active:false,enabled:bool}); Claude's search choice then sends
// {active:true/false,enabled:true} around a native search. OpenRouter's search
// choice sets enabled:true but has no active transitions, and the no-search
// choices set enabled:false. HTTP status stays 200 after the stream opens.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// Resolve the tray choice once. A tray switch while this request is being
	// decoded must not pair the old prompt with the new provider's client.
	provider := s.AskProvider()
	streamer := s.askLLMs[provider]
	model := s.cfg.AskModelFor(provider)
	sw := &statusWriter{ResponseWriter: w}
	w = sw
	outcome := "ok"
	errMsg := ""
	items := 0
	contexts := 0
	history := 0
	screenshots := 0
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
		log.Printf("ask[%s]: model=%s status=%d outcome=%s items=%d contexts=%d history=%d screenshots=%d duration=%s%s",
			providerName, modelName, sw.Status(), outcome, items, contexts, history, screenshots,
			time.Since(start).Round(time.Millisecond), suffix)
	}()

	if !requireType(w, r, "application/json", isJSON) {
		outcome = "bad_content_type"
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body struct {
		Question    string            `json:"question"`
		Items       []prompt.Item     `json:"items"`
		Contexts    []prompt.Context  `json:"contexts"`
		History     []prompt.AskRound `json:"history"`
		Screenshots []string          `json:"screenshots"`
		Timeline    []struct {
			Kind       string `json:"kind"`
			Phrase     string `json:"phrase"`
			Screenshot string `json:"screenshot"`
		} `json:"timeline"`
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
	history = len(body.History)
	screenshots = len(body.Screenshots)

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
	if streamer == nil {
		outcome = "no_provider"
		errMsg = "None of ASK_CLAUDE_API_KEY, ASK_CEREBRAS_API_KEY and ASK_OPENROUTER_API_KEY is set."
		send("error", map[string]any{"message": errMsg})
		return
	}
	if strings.TrimSpace(body.Question) == "" {
		outcome = "no_question"
		errMsg = "No spoken question."
		send("error", map[string]any{"message": errMsg})
		return
	}

	if len(body.Screenshots) > maxAskImages {
		outcome = "too_many_screenshots"
		errMsg = "This Ask card can carry at most 10 screenshots."
		send("error", map[string]any{"message": errMsg})
		return
	}
	images, err := s.shots.getMany(body.Screenshots)
	if err != nil {
		outcome = "missing_screenshot"
		errMsg = "A screenshot is no longer available. Take it again."
		send("error", map[string]any{"message": errMsg})
		return
	}
	if len(images) > maxAskImages {
		outcome = "too_many_screenshots"
		errMsg = "This Ask card can carry at most 10 screenshots."
		send("error", map[string]any{"message": errMsg})
		return
	}
	encodedBytes := 0
	for _, img := range images {
		encodedBytes += len(img.Data)
	}
	if encodedBytes > maxAskImageBase64 {
		outcome = "screenshots_too_large"
		errMsg = "The screenshots are too large for this Ask model. Take fewer or smaller ones."
		send("error", map[string]any{"message": errMsg})
		return
	}
	timeline := make([]prompt.TimelineEntry, 0, len(body.Timeline))
	shotNumber := make(map[string]int, len(body.Screenshots))
	for i, id := range body.Screenshots {
		shotNumber[id] = i + 1
	}
	for _, entry := range body.Timeline {
		switch entry.Kind {
		case "screenshot":
			if n, ok := shotNumber[entry.Screenshot]; ok {
				timeline = append(timeline, prompt.TimelineEntry{Kind: "screenshot", Screenshot: n})
			}
		case "text":
			if phrase := strings.TrimSpace(entry.Phrase); phrase != "" {
				timeline = append(timeline, prompt.TimelineEntry{Kind: "text", Phrase: phrase})
			}
		}
	}
	var p string
	if provider == config.ProviderOpenRouter || provider == config.ProviderClaude {
		p = prompt.BuildAskWeb(body.Question, body.Items, body.Contexts, body.History, len(images), timeline)
	} else {
		p = prompt.BuildAsk(body.Question, body.Items, body.Contexts, body.History, len(images), timeline)
	}
	// Tell the card whether this choice has a search tool at all, so its idle
	// indicator says "Thinking" for the no-search choices instead of guessing
	// from words in the answer.
	searchEnabled := provider == config.ProviderClaude || provider == config.ProviderOpenRouter
	send("search", map[string]any{"active": false, "enabled": searchEnabled})
	onText := func(t string) {
		send("chunk", map[string]any{"text": t})
	}
	// Claude's native search runs before it writes the answer. Tell the card
	// when that happens, so its indicator can say "Searching the web…" during
	// the silent part instead of guessing from a pause in the text.
	onSearch := func(active bool) {
		send("search", map[string]any{"active": active, "enabled": true})
	}
	var result llm.Result
	if len(images) > 0 {
		if searchStreamer, ok := streamer.(llm.SearchImageStreamer); ok {
			result = searchStreamer.StreamImagesWithSearch(ctx, p, images, onText, onSearch)
		} else {
			imageStreamer, ok := streamer.(llm.ImageStreamer)
			if !ok {
				outcome = "no_image_support"
				errMsg = "The selected Ask model does not accept screenshots."
				send("error", map[string]any{"message": errMsg})
				return
			}
			result = imageStreamer.StreamImages(ctx, p, images, onText)
		}
	} else if searchStreamer, ok := streamer.(llm.SearchStreamer); ok {
		result = searchStreamer.StreamWithSearch(ctx, p, onText, onSearch)
	} else {
		result = streamer.Stream(ctx, p, onText)
	}
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
