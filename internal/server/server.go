// Package server is the HTTP layer: stdlib net/http routing and handlers for
// ai-service, the localhost LLM backend for the comart Chrome extension,
// Cairn's Memoir tab and daily-dictation. The server binds 127.0.0.1 and nothing fronts it, so
// loopback on this machine is the trust boundary and there is no auth.
// loopbackOnly and requireType keep out web pages from other sites open in
// this machine's browser.
//
// IT SERVES NO UI. Every route here is JSON or SSE; `GET /` is one line of
// plain text (writeRoot) and there is nothing else that a browser would render.
// The comart front end is the Chrome extension at ~/comart, and Cairn's Memoir tab
// calls the /api/memoir/* routes. Do not add a template, a static directory or an
// embed back here — the clients live elsewhere, and a second copy would have to be
// kept in step with them by hand.
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
	"time"

	"ai-service/internal/config"
	"ai-service/internal/llm"
	"ai-service/internal/mw"
	"ai-service/internal/store"
)

type Server struct {
	cfg config.Config
	mw  *mw.Client
	// explainLLM is /api/explain's model (OpenRouter), memoirLLM is Memoir's
	// (Cerebras). Each is nil when its API key is missing; the handler then
	// reports that instead of the whole server refusing to start, matching how
	// every other handler degrades on a missing secret.
	explainLLM *llm.Client
	memoirLLM  *llm.Client

	httpShort  *http.Client // Groq transcription (bounded)
	httpStream *http.Client // model calls. No timeout: a stream can run for minutes; ctx-cancelled
	httpAzure  *http.Client // Azure verbatim transcription (bounded, generous: files up to ~2 h)

	// inflight counts running handlers, so Drain can wait for handlers that
	// http.Server.Close cut off.
	inflight sync.WaitGroup
}

// Model endpoints. Both speak OpenAI's streaming chat completions.
const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	cerebrasURL   = "https://api.cerebras.ai/v1/chat/completions"
	// maxAnswerTokens bounds one answer, thinking included. Both prompts need
	// far less; the cap only stops a runaway reply.
	maxAnswerTokens = 32000
)

func New(cfg config.Config, st *store.Store) *Server {
	s := &Server{
		cfg:        cfg,
		mw:         mw.New(st),
		httpShort:  &http.Client{Timeout: 120 * time.Second},
		httpStream: &http.Client{}, // no timeout; see the field comment
		httpAzure:  &http.Client{Timeout: 10 * time.Minute},
	}
	if cfg.OpenRouterAPIKey != "" {
		s.explainLLM = llm.New(llm.Config{
			Provider: "OpenRouter",
			URL:      openRouterURL,
			APIKey:   cfg.OpenRouterAPIKey,
			Model:    cfg.ExplainModel,
			Params: map[string]any{
				"max_tokens": maxAnswerTokens,
				// exclude: the thinking still happens but is not streamed back.
				"reasoning": map[string]any{"max_tokens": cfg.ExplainReasoningTokens, "exclude": true},
			},
		}, s.httpStream)
	}
	if cfg.CerebrasAPIKey != "" {
		params := map[string]any{"max_completion_tokens": maxAnswerTokens}
		if cfg.MemoirReasoningEffort == "none" {
			params["disable_reasoning"] = true
		} else {
			params["reasoning_effort"] = cfg.MemoirReasoningEffort
		}
		s.memoirLLM = llm.New(llm.Config{
			Provider: "Cerebras",
			URL:      cerebrasURL,
			APIKey:   cfg.CerebrasAPIKey,
			Model:    cfg.MemoirModel,
			Params:   params,
		}, s.httpStream)
	}
	return s
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
	case p == "/api/memoir/paraphrase" && r.Method == http.MethodPost:
		s.handleMemoirParaphrase(w, r)
	case p == "/" && r.Method == http.MethodGet:
		writeRoot(w)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not found"))
	}
}

// loopbackOnly is the guard ServeHTTP runs before every route. Binding
// 127.0.0.1 keeps other machines out, but not web pages open in this machine's
// browser: a DNS-rebinding page (Host: evil.example:8770) could read every
// response, and any site can fire blind cross-site POSTs that bill the OpenRouter,
// Cerebras and Groq accounts. So the Host must name loopback, and so must an http(s)
// Origin. An absent or "null" Origin, or a non-web one (chrome-extension:// for
// comart, file:// for Cairn's Electron renderer), passes: loopback stays the
// trust boundary, and those are the clients.
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
// passes ok. A web page can send a cross-site POST without a CORS preflight
// only as text/plain, form-urlencoded or multipart/form-data, and an Origin of
// "null" (a sandboxed iframe) gets past loopbackOnly, so demanding the type
// the clients really send closes blind POSTs to the billed routes. comart
// sends application/json and the recorder's audio/* type; Cairn sends
// application/json.
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

// writeRoot answers the bare host with one line of plain text.
//
// THIS IS NOT A PAGE, AND IT IS NOT THE START OF ONE. ai-service serves no HTML,
// no CSS, no fonts and no favicon.
//
// The route exists for two small reasons. "Is it up?" should not need a path
// anybody has to remember: install.sh polls http://127.0.0.1:8770/ as its
// health check, and comart's README uses the same bare URL. And a bare 404
// at the root of a host that is plainly answering reads as a broken deploy
// rather than as an API with nothing at its front door.
//
// Keep it one line of text/plain. There is no CSP here because there is nothing
// to constrain: nosniff is what stops a browser deciding this is markup.
func writeRoot(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write([]byte("ai-service. No UI here — clients are the comart " +
		"Chrome extension, Cairn's Memoir tab and daily-dictation. See /api/* and the README.\n"))
}

// maxJSONBody caps the JSON request bodies of /api/explain and /api/memoir/*.
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

func jsonHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
}
