package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ai-service/internal/llm"
	"ai-service/internal/mw"
	"ai-service/internal/prompt"
)

// ExplainProviders returns the configured explain providers, in the order the
// tray menu shows them. The slice is a copy.
func (s *Server) ExplainProviders() []string {
	return append([]string(nil), s.explainProviders...)
}

// ExplainProvider returns the provider /api/explain is using now.
func (s *Server) ExplainProvider() string {
	if v, ok := s.explainProvider.Load().(string); ok && v != "" {
		return v
	}
	// A Server built by hand in a test has no atomic value; fall back to the
	// configuration it was given.
	return s.cfg.ExplainProvider
}

// SetExplainProvider switches /api/explain to provider. It reports false for a
// provider that is not configured, so a stale saved selection or a bogus menu
// click cannot route an explanation to a service without a key.
func (s *Server) SetExplainProvider(provider string) bool {
	for _, enabled := range s.explainProviders {
		if provider == enabled {
			s.explainProvider.Store(provider)
			return true
		}
	}
	return false
}

// explainStreamer is the client for the currently selected provider.
func (s *Server) explainStreamer() llm.Streamer {
	return s.explainLLMs[s.ExplainProvider()]
}

// handleExplain streams a plain-English explanation of the chosen words as SSE.
// Errors after the stream opens ride as an in-stream `error` event (HTTP status
// stays 200); `done` is always last.
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	provider := s.ExplainProvider()
	model := s.cfg.ExplainModelFor(provider)
	sw := &statusWriter{ResponseWriter: w}
	w = sw
	outcome := "ok"
	errMsg := ""
	items := 0
	prons := 0
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
		log.Printf("explain[%s]: model=%s status=%d outcome=%s items=%d prons=%d duration=%s%s",
			providerName, modelName, sw.Status(), outcome, items, prons,
			time.Since(start).Round(time.Millisecond), suffix)
	}()

	if !requireType(w, r, "application/json", isJSON) {
		outcome = "bad_content_type"
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body struct {
		Text    string        `json:"text"`
		Items   []prompt.Item `json:"items"`
		History []string      `json:"history"`
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
	if len(body.Items) == 0 {
		outcome = "no_items"
		errMsg = "No words selected."
		send("error", map[string]any{"message": errMsg})
		return
	}

	// Pre-fetch Merriam-Webster pronunciation variants for single words.
	var wordsToFetch []string
	seen := map[string]bool{}
	for _, it := range body.Items {
		if len(it.Words) != 1 {
			continue
		}
		wl := strings.ToLower(it.Words[0])
		if seen[wl] {
			continue
		}
		seen[wl] = true
		wordsToFetch = append(wordsToFetch, wl)
	}

	pronBundle := map[string]mw.Bundle{}
	if len(wordsToFetch) > 0 {
		var ok bool
		if pronBundle, ok = s.prefetchPron(ctx, wordsToFetch); !ok {
			outcome = "client_gone"
			return // client gone; the deferred done event is all that is left
		}
		prons = len(pronBundle)
		if len(pronBundle) > 0 {
			send("pron-data", map[string]any{"prons": pronBundle})
		}
	}

	p := prompt.Build(body.Text, body.Items, body.History, pronBundle)
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

// Bounds on the Merriam-Webster prefetch. A burst of parallel lookups invites
// Cloudflare's challenge, and one black-holed lookup can run for over a minute
// (a 20 s client timeout, several attempts, a cross-reference to follow), so
// the explanation waits for MW only so long before going on without it.
const (
	mwConcurrency = 4
	mwBudget      = 10 * time.Second
)

// prefetchPron looks up MW pronunciation variants for words. Cached words are
// answered at once; the rest go to MW at most mwConcurrency at a time, and the
// bundles that arrived within mwBudget are returned. ok is false when ctx ended
// first. Lookups still running when it returns finish in the background —
// their results still land in mw's cache — but none is used for this request,
// and no new one starts.
func (s *Server) prefetchPron(ctx context.Context, words []string) (bundles map[string]mw.Bundle, ok bool) {
	var (
		mu      sync.Mutex
		results = map[string]mw.Bundle{}
		allDone = make(chan struct{})
		quit    = make(chan struct{})
	)
	defer close(quit)

	go func() {
		// Cache hits first, so a word MW already answered never waits for a
		// slot behind slow network lookups.
		var misses []string
		for _, word := range words {
			select {
			case <-quit:
				return
			default:
			}
			if b, hit := s.mw.CachedBundle(word); hit {
				if b != nil {
					mu.Lock()
					results[word] = *b
					mu.Unlock()
				}
				continue
			}
			misses = append(misses, word)
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, mwConcurrency)
		for _, word := range misses {
			select {
			case sem <- struct{}{}:
			case <-quit:
				return
			}
			select {
			case <-quit:
				return // the slot won a tie with quit; do not start another lookup
			default:
			}
			wg.Add(1)
			go func(word string) {
				defer func() {
					<-sem
					wg.Done()
				}()
				b, err := s.mw.BundleVariants(word)
				if err != nil {
					log.Printf("mw[%s] threw: %v", word, err)
					return
				}
				if b != nil {
					mu.Lock()
					results[word] = *b
					mu.Unlock()
				}
			}(word)
		}
		wg.Wait()
		close(allDone)
	}()

	timer := time.NewTimer(mwBudget)
	defer timer.Stop()
	select {
	case <-allDone:
	case <-timer.C:
		log.Printf("mw: prefetch budget (%s) spent; explaining without the rest", mwBudget)
	case <-ctx.Done():
		return nil, false
	}

	mu.Lock()
	defer mu.Unlock()
	bundles = make(map[string]mw.Bundle, len(results))
	for word, b := range results {
		bundles[word] = b
	}
	return bundles, true
}
