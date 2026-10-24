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

	"ai-service/internal/mw"
	"ai-service/internal/prompt"
)

// handleExplain streams a plain-English explanation of the chosen words as SSE.
// Errors after the stream opens ride as an in-stream `error` event (HTTP status
// stays 200); `done` is always last.
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "application/json", isJSON) {
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
			jsonError(w, "Request too large.", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "Invalid JSON.", http.StatusBadRequest)
		return
	}

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

	if s.explainLLM == nil {
		send("error", map[string]any{"message": "Neither OPENROUTER_API_KEY nor CLAUDE_API_KEY is set."})
		return
	}
	if len(body.Items) == 0 {
		send("error", map[string]any{"message": "No words selected."})
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
			return // client gone; the deferred done event is all that is left
		}
		if len(pronBundle) > 0 {
			send("pron-data", map[string]any{"prons": pronBundle})
		}
	}

	p := prompt.Build(body.Text, body.Items, body.History, pronBundle)
	result := s.explainLLM.Stream(ctx, p, func(t string) {
		send("chunk", map[string]any{"text": t})
	})
	if !result.OK && ctx.Err() == nil {
		send("error", map[string]any{"message": result.UserMessage()})
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
