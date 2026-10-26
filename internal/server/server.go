// Package server is the HTTP layer: stdlib net/http routing and handlers.
// It serves no UI, only JSON and SSE. The clients live in ~/comart, ~/cairn and
// ~/daily-dictation; README.md has the routes and the security model.
package server

import (
	"context"
	"encoding/json"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-service/internal/config"
	"ai-service/internal/llm"
	"ai-service/internal/mw"
	"ai-service/internal/store"
)

type Server struct {
	cfg config.Config
	mw  *mw.Client
	// explainLLM is /api/explain's model (OpenRouter or Claude), memoirLLM is
	// Memoir's (Cerebras or Claude). Whichever key config.Load found picks the
	// provider, and it refuses both keys of the explain or Memoir route. Each is nil
	// when its API key is missing; the handler then reports that instead of the
	// whole server refusing to start.
	explainLLM llm.Streamer
	memoirLLM  llm.Streamer

	httpShort      *http.Client // /api/transcribe: Groq (2 min)
	httpOpenRouter *http.Client // OpenRouter MAI: transcribe and verbatim (10 min backstop)

	// transcribeProviders is every provider config.Load found (Groq first),
	// and transcribeProvider is the one /api/transcribe uses now. The tray
	// picker may change it while requests are running, hence atomic.
	transcribeProviders []string
	transcribeProvider  atomic.Value // string

	// inflight counts running handlers, so Drain can wait for handlers that
	// http.Server.Close cut off.
	inflight sync.WaitGroup
}

// Model endpoints. OpenRouter and Cerebras speak OpenAI's streaming chat
// completions; Claude is the Messages API at its API root.
const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	cerebrasURL   = "https://api.cerebras.ai/v1/chat/completions"
	claudeURL     = "https://api.anthropic.com"
	// maxAnswerTokens bounds one answer, thinking included. Both prompts need
	// far less; the cap only stops a runaway reply.
	maxAnswerTokens = 32000
)

func New(cfg config.Config, st *store.Store) *Server {
	s := &Server{
		cfg:                 cfg,
		mw:                  mw.New(st),
		httpShort:           &http.Client{Timeout: 120 * time.Second},
		httpOpenRouter:      &http.Client{Timeout: 10 * time.Minute},
		transcribeProviders: append([]string(nil), cfg.TranscribeProviders...),
	}
	if cfg.TranscribeProvider != "" {
		s.transcribeProvider.Store(cfg.TranscribeProvider)
	}
	// Model calls have no overall timeout: a stream can run for minutes and
	// the request context cancels it.
	hc := &http.Client{}
	s.explainLLM = newExplainLLM(cfg, hc)
	s.memoirLLM = newMemoirLLM(cfg, hc)
	return s
}

func newClaude(key, model, effort, route string, hc *http.Client) llm.Streamer {
	return llm.NewClaude(llm.ClaudeConfig{
		APIKey:    key,
		BaseURL:   claudeURL,
		Model:     model,
		MaxTokens: maxAnswerTokens,
		Effort:    effort,
		Route:     route,
	}, hc)
}

func newExplainLLM(cfg config.Config, hc *http.Client) llm.Streamer {
	switch cfg.ExplainProvider {
	case config.ProviderClaude:
		return newClaude(cfg.ClaudeAPIKey, cfg.ExplainModel, cfg.ExplainEffort, "explain", hc)
	case config.ProviderOpenRouter:
		return llm.New(llm.Config{
			Provider: "OpenRouter",
			URL:      openRouterURL,
			APIKey:   cfg.OpenRouterAPIKey,
			Model:    cfg.ExplainModel,
			Params: map[string]any{
				"max_tokens": maxAnswerTokens,
				// exclude: the thinking still happens but is not streamed back.
				"reasoning": map[string]any{"max_tokens": cfg.ExplainReasoningTokens, "exclude": true},
			},
		}, hc)
	}
	return nil
}

func newMemoirLLM(cfg config.Config, hc *http.Client) llm.Streamer {
	switch cfg.MemoirProvider {
	case config.ProviderClaude:
		return newClaude(cfg.MemoirClaudeAPIKey, cfg.MemoirModel, cfg.MemoirEffort, "memoir", hc)
	case config.ProviderCerebras:
		params := map[string]any{"max_completion_tokens": maxAnswerTokens}
		if cfg.MemoirReasoningEffort == "none" {
			params["disable_reasoning"] = true
		} else {
			params["reasoning_effort"] = cfg.MemoirReasoningEffort
		}
		return llm.New(llm.Config{
			Provider: "Cerebras",
			URL:      cerebrasURL,
			APIKey:   cfg.MemoirCerebrasAPIKey,
			Model:    cfg.MemoirModel,
			Params:   params,
		}, hc)
	}
	return nil
}

// Drain waits, within ctx, for the handlers still running. main calls this
// after http.Server.Shutdown (or Close), so the cache is not closed under a
// handler that Close cut off but that has not returned yet.
func (s *Server) Drain(ctx context.Context) {
	handlersDone := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
	case <-ctx.Done():
		log.Printf("drain timed out with request handlers still running")
	}
}

// ServeHTTP dispatches on method + path, with a plain-text "Not found"
// fallback. No auth middleware — that is the point; loopbackOnly below is a
// browser-origin check, not a login.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Done()
	if !loopbackOnly(r) {
		jsonError(w, "Forbidden.", http.StatusForbidden)
		return
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/api/transcribe":
		s.handleTranscribe(w, r)
	case r.Method == http.MethodPost && p == "/api/transcribe-verbatim":
		s.handleTranscribeVerbatim(w, r)
	case p == "/api/explain" && r.Method == http.MethodPost:
		s.handleExplain(w, r)
	case p == "/api/pron" && r.Method == http.MethodGet:
		s.handlePron(w, r)
	case p == "/api/credits" && r.Method == http.MethodGet:
		s.handleCredits(w, r)
	case p == "/api/memoir/check" && r.Method == http.MethodPost:
		s.handleMemoirCheck(w, r)
	case p == "/" && r.Method == http.MethodGet:
		writeRoot(w)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not found"))
	}
}

// loopbackOnly is the guard ServeHTTP runs before every route. Binding
// 127.0.0.1 keeps other machines out, but not pages open in this machine's
// browser: DNS rebinding could read responses, and any site can fire blind
// cross-site POSTs that bill the OpenRouter, Claude, Cerebras or Groq
// accounts. The Host and any http(s) Origin must name loopback; an absent or
// "null" Origin, or a non-web one (chrome-extension:// for comart, file:// for
// Cairn's Electron renderer), passes: loopback stays the trust boundary.
func loopbackOnly(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host != "" && !isLoopbackName(host) {
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false // no browser sends a malformed Origin
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return true
	}
	return isLoopbackName(u.Hostname())
}

// requireType answers 415 and returns false unless the request's media type
// passes ok. A browser can send a cross-site POST without a preflight only as
// text/plain, form-urlencoded or multipart, and a sandboxed iframe's "null"
// Origin gets past loopbackOnly, so demanding the clients' real types closes
// blind POSTs to the billed routes: JSON from comart and Cairn, audio/* (or
// video/*) from comart's recorder, and multipart from daily-dictation.
func requireType(w http.ResponseWriter, r *http.Request, want string, ok func(mediaType string) bool) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ok(mt) {
		return true
	}
	jsonError(w, "Content-Type must be "+want+".", http.StatusUnsupportedMediaType)
	return false
}

func isJSON(mt string) bool { return mt == "application/json" }

func isLoopbackName(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// writeRoot answers the bare host with one line of text/plain. install.sh and
// the clients use it as a health check, and a bare 404 there reads as a broken
// deploy. It is deliberately not a page: no HTML, CSS, fonts or favicon. nosniff
// is what stops a browser deciding this line is markup.
func writeRoot(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write([]byte("ai-service. No UI here — clients are the comart " +
		"Chrome extension, Cairn's Memoir tab and daily-dictation. See /api/* and the README.\n"))
}

// maxJSONBody caps the JSON request bodies of /api/explain and /api/memoir/check.
const maxJSONBody = 5 * 1024 * 1024

// writeJSON serialises v as JSON with Cache-Control: no-store.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"error":"internal"}`)
		status = http.StatusInternalServerError
	}
	jsonHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func jsonError(w http.ResponseWriter, message string, status int) {
	writeJSON(w, status, map[string]any{"error": message})
}

// jsonErrorDetail is jsonError with a provider or transport detail attached.
func jsonErrorDetail(w http.ResponseWriter, message string, status int, detail string) {
	writeJSON(w, status, map[string]any{"error": message, "detail": detail})
}

func jsonHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
}
