package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ai-service/internal/config"
	"ai-service/internal/llm"
	"ai-service/internal/memoir"
	"ai-service/internal/mw"
)

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	return body.Error
}

func TestLoopbackGuard(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name, host, origin string
		allowed            bool
	}{
		{"no Origin", "127.0.0.1:8770", "", true},
		{"null Origin", "127.0.0.1:8770", "null", true},
		{"comart extension", "127.0.0.1:8770", "chrome-extension://abc", true},
		{"moz extension", "127.0.0.1:8770", "moz-extension://abc", true},
		{"file page", "127.0.0.1:8770", "file://", true},
		{"loopback dev server", "127.0.0.1:8770", "http://127.0.0.1:5173", true},
		{"localhost origin", "127.0.0.1:8770", "https://LocalHost:3000", true},
		{"ipv6 origin", "127.0.0.1:8770", "http://[::1]:8080", true},
		{"Host localhost", "localhost:8770", "", true},
		{"Host localhost mixed case", "LOCALHOST:8770", "", true},
		{"Host ipv6", "[::1]:8770", "", true},
		{"Host without port", "127.0.0.1", "", true},
		{"Host ipv6 without port", "[::1]", "", true},
		{"empty Host", "", "", true},

		{"rebinding Host", "evil.example:8770", "", false},
		{"rebinding Host without port", "evil.example", "", false},
		{"loopback-looking Host", "127.0.0.1.evil.example:8770", "", false},
		{"cross-site Origin", "127.0.0.1:8770", "https://evil.example", false},
		{"cross-site Origin on our port", "127.0.0.1:8770", "http://evil.example:8770", false},
		{"loopback-looking Origin", "127.0.0.1:8770", "http://127.0.0.1.evil.example", false},
		{"malformed Origin", "127.0.0.1:8770", "http://[::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if tc.allowed {
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403 (body %q)", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec); got != "Forbidden." {
				t.Fatalf("error %q, want %q", got, "Forbidden.")
			}
		})
	}
}

// The guard runs before routing, so a blind cross-site POST never reaches a
// handler that would spend an account.
func TestLoopbackGuardCoversPosts(t *testing.T) {
	s := &Server{}
	for _, path := range []string{"/api/transcribe/stream", "/api/transcribe-verbatim", "/api/explain", "/api/ask", "/api/screenshot", "/api/screenshot/release", "/api/memoir/check"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Host = "127.0.0.1:8770"
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", path, rec.Code)
		}
	}
}

func TestUTF16Len(t *testing.T) {
	for s, want := range map[string]int{
		"":         0,
		"abc":      3,
		"’":        1, // 3 UTF-8 bytes, one UTF-16 unit
		"é":        1,
		"😀":        2, // outside the BMP: a surrogate pair in JS
		"a’😀":      4,
		"\xff\xfe": 2, // invalid UTF-8: one U+FFFD per byte
	} {
		if got := utf16Len(s); got != want {
			t.Errorf("utf16Len(%q) = %d, want %d", s, got, want)
		}
	}
}

func postJSON(t *testing.T, s *Server, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// The cap counts what the clients count (JS String.length), not UTF-8 bytes.
// With a nil memoirLLM, a request that passes the length check stops at 503.
func TestMemoirCheckCapCountsUTF16(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name, text string
		wantStatus int
		wantError  string
	}{
		{"12000 curly quotes", strings.Repeat("’", 12000), http.StatusServiceUnavailable, "Neither MEMOIR_CEREBRAS_API_KEY nor MEMOIR_CLAUDE_API_KEY is set."},
		{"12001 curly quotes", strings.Repeat("’", 12001), http.StatusBadRequest, "entry too long (max 12000 chars)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, s, "/api/memoir/check", map[string]string{"content": tc.text})
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := decodeError(t, rec); got != tc.wantError {
				t.Fatalf("error %q, want %q", got, tc.wantError)
			}
		})
	}
}

func TestExplainBodyCap(t *testing.T) {
	s := &Server{}

	big := `{"text":"` + strings.Repeat("a", maxJSONBody) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/explain", strings.NewReader(big))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: status %d, want 413", rec.Code)
	}
	if got := decodeError(t, rec); got != "Request too large." {
		t.Fatalf("oversize: error %q", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/explain", strings.NewReader(`{"text":`))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: status %d, want 400", rec.Code)
	}
	if got := decodeError(t, rec); got != "Invalid JSON." {
		t.Fatalf("malformed: error %q", got)
	}
}

// fakeCache stands in for the store behind mw: every word is a cache hit with
// one pronunciation, so a lookup never reaches the network. hold, when set,
// blocks each Get until it is closed.
type fakeCache struct {
	hold     chan struct{}
	entered  chan struct{}
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (c *fakeCache) Get(key string) (string, bool) {
	n := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		m := c.maxSeen.Load()
		if n <= m || c.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
	}
	if c.hold != nil {
		<-c.hold
	} else {
		time.Sleep(20 * time.Millisecond)
	}
	word := strings.TrimPrefix(key, "mw:")
	b, _ := json.Marshal(map[string]any{
		"t": time.Now().UnixMilli(),
		"entries": []map[string]any{{
			"hword": word, "pos": "noun",
			"prons": []map[string]any{{"ipa": "ipa-" + word}},
		}},
	})
	return string(b), true
}

func (c *fakeCache) Set(string, string, int64) {}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in tests")
}

// fakeModel answers a chat completion with reply as the model's whole answer,
// streamed the way OpenRouter and Cerebras stream it.
type fakeModel struct{ reply string }

func (f fakeModel) RoundTrip(req *http.Request) (*http.Response, error) {
	chunk, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]string{"content": f.reply}, "finish_reason": "stop"}},
	})
	body := "data: " + string(chunk) + "\n\ndata: [DONE]\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// captureModel answers like fakeModel and keeps the request body, so a test
// can read the prompt the handler actually assembled.
type captureModel struct {
	reply string
	body  atomic.Value
}

func (m *captureModel) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	m.body.Store(string(b))
	return fakeModel{reply: m.reply}.RoundTrip(req)
}

// testLLM builds a model client over transport.
func testLLM(transport http.RoundTripper) *llm.Client {
	return llm.New(llm.Config{Provider: "Test", URL: "https://test.invalid/v1/chat/completions", Model: "test"},
		&http.Client{Transport: transport})
}

// explainServer builds a Server whose model client fails fast without the
// network and whose MW client answers from cache.
func explainServer(t *testing.T, cache mw.Cache) *Server {
	t.Helper()
	return &Server{
		cfg:              config.Config{ExplainProvider: config.ProviderOpenRouter},
		explainProviders: []string{config.ProviderOpenRouter},
		explainLLMs: map[string]llm.Streamer{
			config.ProviderOpenRouter: testLLM(failingTransport{}),
		},
		mw: mw.New(cache),
	}
}

func explainRequest(ctx context.Context, words int) *http.Request {
	items := make([]string, words)
	for i := range items {
		items[i] = fmt.Sprintf(`{"words":["w%d"]}`, i)
	}
	body := `{"text":"t","items":[` + strings.Join(items, ",") + `]}`
	req := httptest.NewRequest(http.MethodPost, "/api/explain", strings.NewReader(body)).WithContext(ctx)
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestExplainPrefetchLimitsConcurrency(t *testing.T) {
	cache := &fakeCache{}
	s := explainServer(t, cache)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, explainRequest(context.Background(), 12))

	if got := cache.maxSeen.Load(); got > mwConcurrency {
		t.Errorf("%d MW lookups ran at once, want at most %d", got, mwConcurrency)
	}
	out := rec.Body.String()
	i := strings.Index(out, "event: pron-data\ndata: ")
	if i < 0 {
		t.Fatalf("no pron-data event in %q", out)
	}
	line := out[i+len("event: pron-data\ndata: "):]
	line = line[:strings.Index(line, "\n")]
	var data struct {
		Prons map[string]mw.Bundle `json:"prons"`
	}
	if err := json.Unmarshal([]byte(line), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Prons) != 12 {
		t.Errorf("pron-data carries %d words, want 12", len(data.Prons))
	}
	if !strings.HasSuffix(out, "event: done\ndata: {}\n\n") {
		t.Errorf("stream does not end with done: %q", out)
	}
}

// A client that goes away while MW is still looking up words ends the request
// at once instead of waiting on the lookups.
func TestExplainPrefetchStopsOnCancel(t *testing.T) {
	cache := &fakeCache{hold: make(chan struct{}), entered: make(chan struct{}, 1)}
	s := explainServer(t, cache)
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.ServeHTTP(rec, explainRequest(ctx, 8))
	}()
	<-cache.entered
	cancel()

	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still waiting on MW after the client went away")
	}
	close(cache.hold) // let the parked lookups finish

	if out := rec.Body.String(); out != "event: done\ndata: {}\n\n" {
		t.Errorf("body %q, want only the done event", out)
	}
	if got := cache.maxSeen.Load(); got > mwConcurrency {
		t.Errorf("%d MW lookups ran at once, want at most %d", got, mwConcurrency)
	}
}

// /api/ask streams the model's answer and puts the spoken question plus the
// picked passage into the prompt. It must not reuse /api/explain's vocabulary
// skeleton: the question is the instruction, the page is data.
func TestAskStreamsAnswerFromQuestionAndContext(t *testing.T) {
	m := &captureModel{reply: "It means the officer formally accuses the suspect."}
	s := &Server{
		cfg:          config.Config{AskProvider: config.ProviderCerebras},
		askProviders: []string{config.ProviderCerebras},
		askLLMs: map[string]llm.Streamer{
			config.ProviderCerebras: testLLM(m),
		},
	}
	body := `{"question":"What does charge mean here?","items":[{"phrase":"charge","words":["charge"]}],"contexts":[{"phrase":"charge","text":"The officer will charge the suspect at noon."}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(body))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `event: chunk`) || !strings.Contains(out, "formally accuses") {
		t.Fatalf("answer not streamed: %q", out)
	}
	if !strings.HasSuffix(out, "event: done\ndata: {}\n\n") {
		t.Fatalf("stream does not end with done: %q", out)
	}
	promptReq, _ := m.body.Load().(string)
	for _, want := range []string{"What does charge mean here?", "The officer will charge the suspect at noon."} {
		if !strings.Contains(promptReq, want) {
			t.Errorf("model request missing %q", want)
		}
	}
}

// A follow-up sends the earlier question/answer pairs to /api/ask, and the
// prompt carries them before the new question so references resolve.
func TestAskStreamsAnswerWithEarlierConversation(t *testing.T) {
	m := &captureModel{reply: "Yes — by the second reading."}
	s := &Server{
		cfg:          config.Config{AskProvider: config.ProviderCerebras},
		askProviders: []string{config.ProviderCerebras},
		askLLMs: map[string]llm.Streamer{
			config.ProviderCerebras: testLLM(m),
		},
	}
	body := `{"question":"Which one did you mean?","history":[{"question":"What are the two readings?","answer":"A narrow one and a broad one."}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(body))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	promptReq, _ := m.body.Load().(string)
	for _, want := range []string{
		"# EARLIER CONVERSATION",
		"What are the two readings?",
		"A narrow one and a broad one.",
		"Which one did you mean?",
	} {
		if !strings.Contains(promptReq, want) {
			t.Errorf("model request missing %q", want)
		}
	}
}

// The OpenRouter Ask client is DeepSeek V4.1 Flash 1017 at thinking level
// high, and it carries the web search server tool. Its reasoning control is
// effort, not explain's token budget, and the tool runs on Exa because
// DeepSeek has no native search.
func TestAskOpenRouterUsesDeepSeekHighEffortWithWebSearch(t *testing.T) {
	m := &captureModel{reply: "ok"}
	cfg := config.Config{
		AskProviders:        []string{config.ProviderOpenRouter},
		AskOpenRouterAPIKey: "or",
	}
	clients := newAskLLMs(cfg, &http.Client{Transport: m})
	streamer := clients[config.ProviderOpenRouter]
	if streamer == nil {
		t.Fatal("no OpenRouter ask client")
	}
	if res := streamer.Stream(context.Background(), "hi", nil); !res.OK {
		t.Fatalf("stream failed: %+v", res)
	}

	raw, _ := m.body.Load().(string)
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("request body %q: %v", raw, err)
	}
	if got := body["model"]; got != "deepseek/deepseek-v4.1-flash-1017" {
		t.Errorf("model %v", got)
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["exclude"] != true {
		t.Errorf("reasoning %v", body["reasoning"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools %v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "openrouter:web_search" {
		t.Errorf("tool %v", tool)
	}
	params, _ := tool["parameters"].(map[string]any)
	if params["engine"] != "exa" || params["max_results"] != float64(10) || params["max_uses"] != float64(2) {
		t.Errorf("web search parameters %v", params)
	}
}

// Claude's web search tool belongs to /api/ask only. The Explain client must
// keep its old tool-less request.
func TestClaudeWebSearchIsAskOnly(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	cases := []struct {
		name   string
		stream func(*http.Client) llm.Streamer
	}{
		{"ask", func(hc *http.Client) llm.Streamer {
			cfg := config.Config{AskProviders: []string{config.ProviderClaude}, AskClaudeAPIKey: "k"}
			return newAskLLMs(cfg, hc)[config.ProviderClaude]
		}},
		{"ask no search", func(hc *http.Client) llm.Streamer {
			cfg := config.Config{AskProviders: []string{config.ProviderClaudeNoSearch}, AskClaudeAPIKey: "k"}
			return newAskLLMs(cfg, hc)[config.ProviderClaudeNoSearch]
		}},
		{"explain", func(hc *http.Client) llm.Streamer {
			cfg := config.Config{ExplainProviders: []string{config.ProviderClaude}, ClaudeAPIKey: "k"}
			return newExplainLLMs(cfg, hc)[config.ProviderClaude]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &claudeToolCapture{body: body}
			streamer := tc.stream(&http.Client{Transport: capture})
			if res := streamer.Stream(context.Background(), "hi", nil); !res.OK {
				t.Fatalf("stream failed: %+v", res)
			}
			raw, _ := capture.sent.Load().(string)
			var req map[string]any
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				t.Fatalf("request body %q: %v", raw, err)
			}
			tools, _ := req["tools"].([]any)
			if tc.name == "ask" {
				if len(tools) != 1 {
					t.Fatalf("ask tools %v", req["tools"])
				}
				tool, _ := tools[0].(map[string]any)
				if tool["type"] != "web_search_20250305" || tool["max_uses"] != float64(2) {
					t.Errorf("ask tool %v", tool)
				}
				return
			}
			if len(tools) != 0 {
				t.Errorf("explain request carries tools %v", req["tools"])
			}
		})
	}
}

// /api/ask turns Claude's search callbacks into SSE `search` events, and the
// prompt carries the web-search guidance.
func TestAskSendsClaudeSearchStatus(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"the answer"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	capture := &claudeToolCapture{body: body}
	cfg := config.Config{AskProvider: config.ProviderClaude, AskProviders: []string{config.ProviderClaude}, AskClaudeAPIKey: "k"}
	s := &Server{
		cfg:          cfg,
		askProviders: cfg.AskProviders,
		askLLMs:      newAskLLMs(cfg, &http.Client{Transport: capture}),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"question":"latest news"}`))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	out := rec.Body.String()
	for _, want := range []string{
		`event: search`,
		`"active":true`,
		`"active":false`,
		`"enabled":true`,
		`event: chunk`,
		`the answer`,
		`event: done`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, `"active":true`); got != 1 {
		t.Errorf("stream has %d active=true events, want 1:\n%s", got, out)
	}
	if got := strings.Count(out, `"active":false`); got != 2 {
		t.Errorf("stream has %d active=false events, want 2 (initial + result):\n%s", got, out)
	}
	if strings.Index(out, "event: search") > strings.Index(out, "event: chunk") {
		t.Errorf("search event came after the first chunk:\n%s", out)
	}
	if !strings.HasSuffix(out, "event: done\ndata: {}\n\n") {
		t.Errorf("stream does not end with done:\n%s", out)
	}
	raw, _ := capture.sent.Load().(string)
	if !strings.Contains(raw, "# WEB SEARCH") {
		t.Errorf("request prompt lacks the web-search section: %s", raw)
	}
}

// claudeToolCapture answers a Messages API streaming request and records the
// JSON body, so a test can see which tools the server built.
type claudeToolCapture struct {
	body string
	sent atomic.Value
}

func (c *claudeToolCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	c.sent.Store(string(b))
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Request:    req,
	}, nil
}

// The no-search Ask choice keeps DeepSeek's model and high reasoning but sends
// no tools array at all.
func TestAskOpenRouterNoSearchOmitsTools(t *testing.T) {
	m := &captureModel{reply: "ok"}
	cfg := config.Config{
		AskProviders:        []string{config.ProviderOpenRouterNoSearch},
		AskOpenRouterAPIKey: "or",
	}
	streamer := newAskLLMs(cfg, &http.Client{Transport: m})[config.ProviderOpenRouterNoSearch]
	if streamer == nil {
		t.Fatal("no OpenRouter no-search ask client")
	}
	if res := streamer.Stream(context.Background(), "hi", nil); !res.OK {
		t.Fatalf("stream failed: %+v", res)
	}
	raw, _ := m.body.Load().(string)
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("request body %q: %v", raw, err)
	}
	if body["model"] != "deepseek/deepseek-v4.1-flash-1017" {
		t.Errorf("model %v", body["model"])
	}
	if reasoning, _ := body["reasoning"].(map[string]any); reasoning["effort"] != "high" {
		t.Errorf("reasoning %v", body["reasoning"])
	}
	if _, ok := body["tools"]; ok {
		t.Errorf("no-search request carries tools %v", body["tools"])
	}
}

// The no-search OpenRouter choice goes through /api/ask with the plain Ask
// prompt, no tools, and an initial search event saying search is unavailable.
func TestAskOpenRouterNoSearchUsesPlainPrompt(t *testing.T) {
	m := &captureModel{reply: "plain answer"}
	cfg := config.Config{
		AskProvider:         config.ProviderOpenRouterNoSearch,
		AskProviders:        []string{config.ProviderOpenRouterNoSearch},
		AskOpenRouterAPIKey: "or",
	}
	s := &Server{
		cfg:          cfg,
		askProviders: cfg.AskProviders,
		askLLMs:      newAskLLMs(cfg, &http.Client{Transport: m}),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"question":"what is this?"}`))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, `"enabled":false`) || !strings.Contains(out, "plain answer") {
		t.Errorf("stream %q", out)
	}
	raw, _ := m.body.Load().(string)
	if strings.Contains(raw, "# WEB SEARCH") {
		t.Errorf("no-search prompt carries web-search guidance: %s", raw)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("request body %q: %v", raw, err)
	}
	if _, ok := body["tools"]; ok {
		t.Errorf("no-search request carries tools %v", body["tools"])
	}
}

// A web page can POST cross-site without a preflight only with text/plain or a
// form type, and a sandboxed iframe's Origin is "null", which the guard lets
// through. The billed routes therefore demand the type the clients really send.
func TestBilledRoutesDemandTheClientsContentType(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct{ path, ctype string }{
		{"/api/explain", "text/plain;charset=UTF-8"},
		{"/api/explain", ""},
		{"/api/ask", "text/plain;charset=UTF-8"},
		{"/api/ask", ""},
		{"/api/screenshot", "text/plain;charset=UTF-8"},
		{"/api/screenshot", ""},
		{"/api/screenshot/release", "text/plain;charset=UTF-8"},
		{"/api/screenshot/release", ""},
		{"/api/memoir/check", "application/x-www-form-urlencoded"},
		{"/api/transcribe/stream", "text/plain"},
		{"/api/transcribe/stream", ""},
		{"/api/transcribe-verbatim", "application/json"},
		{"/api/transcribe-verbatim", ""},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
		req.Host = "127.0.0.1:8770"
		req.Header.Set("Origin", "null")
		if tc.ctype != "" {
			req.Header.Set("Content-Type", tc.ctype)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s with %q: status %d, want 415", tc.path, tc.ctype, rec.Code)
		}
	}
	// What comart's recorder sends still reaches the handler (which then
	// reports the missing key, since this Server has none).
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe/stream", strings.NewReader("x"))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "audio/webm;codecs=opus")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Errorf("audio/webm;codecs=opus rejected")
	}
}

// Drain waits for a running handler before it returns.
func TestDrainWaitsForHandlers(t *testing.T) {
	cache := &fakeCache{hold: make(chan struct{}), entered: make(chan struct{}, 1)}
	s := explainServer(t, cache)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ServeHTTP(httptest.NewRecorder(), explainRequest(ctx, 1))
	<-cache.entered

	short, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	start := time.Now()
	s.Drain(short)
	if time.Since(start) < 40*time.Millisecond {
		t.Error("Drain returned while a handler was still running")
	}

	cancel()
	close(cache.hold)
	done, stop2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop2()
	start = time.Now()
	s.Drain(done)
	if time.Since(start) > 4*time.Second {
		t.Error("Drain did not return after the handler finished")
	}
}

// TestMemoirCheckKeepsEveryIssue: the prompt asks for every mistake, so the
// handler passes a long list through whole; CheckMaxIssues is only a bound.
func TestMemoirCheckKeepsEveryIssue(t *testing.T) {
	const content = "An entry long enough to pass the sanity floor."
	for _, tc := range []struct{ sent, want int }{
		{120, 120},
		{memoir.CheckMaxIssues + 1, memoir.CheckMaxIssues},
	} {
		t.Run(fmt.Sprint(tc.sent), func(t *testing.T) {
			issues := make([]memoir.CheckIssue, tc.sent)
			for i := range issues {
				issues[i] = memoir.CheckIssue{Category: "grammar", Original: fmt.Sprint("w", i), Suggestion: fmt.Sprint("x", i), Reason: "r"}
			}
			reply, _ := json.Marshal(map[string]any{"corrected": content, "issues": issues})
			s := &Server{memoirLLM: testLLM(fakeModel{reply: string(reply)})}
			rec := postJSON(t, s, "/api/memoir/check", map[string]string{"content": content})
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d (body %q)", rec.Code, rec.Body.String())
			}
			var body struct {
				Issues []memoir.CheckIssue `json:"issues"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Issues) != tc.want {
				t.Fatalf("got %d issues, want %d", len(body.Issues), tc.want)
			}
		})
	}
}

func TestNewBuildsExplainClientsPerProvider(t *testing.T) {
	s := New(config.Config{
		OpenRouterAPIKey: "or",
		ClaudeAPIKey:     "cl",
		ExplainProviders: []string{config.ProviderClaude, config.ProviderOpenRouter},
		ExplainProvider:  config.ProviderClaude,
	}, nil)

	if got := s.ExplainProviders(); len(got) != 2 || got[0] != config.ProviderClaude || got[1] != config.ProviderOpenRouter {
		t.Fatalf("providers %v", got)
	}
	if got := s.ExplainProvider(); got != config.ProviderClaude {
		t.Fatalf("provider %q, want %q", got, config.ProviderClaude)
	}
	if got := fmt.Sprintf("%T", s.explainLLMs[config.ProviderClaude]); got != "*llm.Claude" {
		t.Errorf("claude client is %s", got)
	}
	if got := fmt.Sprintf("%T", s.explainLLMs[config.ProviderOpenRouter]); got != "*llm.Client" {
		t.Errorf("openrouter client is %s", got)
	}
	if !s.SetExplainProvider(config.ProviderOpenRouter) {
		t.Error("could not select the configured OpenRouter provider")
	}
	if got := s.ExplainProvider(); got != config.ProviderOpenRouter {
		t.Errorf("provider %q after switch, want %q", got, config.ProviderOpenRouter)
	}
	if s.SetExplainProvider("not-configured") {
		t.Error("selected a provider that has no key")
	}
}

func TestNewBuildsAskClientsPerProvider(t *testing.T) {
	s := New(config.Config{
		AskClaudeAPIKey:   "cl",
		AskCerebrasAPIKey: "ce",
		AskProviders:      []string{config.ProviderClaude, config.ProviderCerebras},
		AskProvider:       config.ProviderClaude,
	}, nil)

	if got := s.AskProviders(); len(got) != 2 || got[0] != config.ProviderClaude || got[1] != config.ProviderCerebras {
		t.Fatalf("providers %v", got)
	}
	if got := s.AskProvider(); got != config.ProviderClaude {
		t.Fatalf("provider %q, want %q", got, config.ProviderClaude)
	}
	if got := fmt.Sprintf("%T", s.askLLMs[config.ProviderClaude]); got != "*llm.Claude" {
		t.Errorf("claude client is %s", got)
	}
	if got := fmt.Sprintf("%T", s.askLLMs[config.ProviderCerebras]); got != "*llm.Client" {
		t.Errorf("cerebras client is %s", got)
	}
	if !s.SetAskProvider(config.ProviderCerebras) {
		t.Error("could not select the configured Cerebras provider")
	}
	if got := s.AskProvider(); got != config.ProviderCerebras {
		t.Errorf("provider %q after switch, want %q", got, config.ProviderCerebras)
	}
	if s.SetAskProvider("not-configured") {
		t.Error("selected a provider that has no key")
	}
}

// New builds Memoir's client from the provider config.Load picked, and leaves
// it nil (so the handlers report the missing key) when neither key is set.
func TestNewBuildsMemoirClientPerProvider(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.Config
		wantNil bool
		wantTyp string
	}{
		{"no key", config.Config{}, true, ""},
		{"cerebras", config.Config{MemoirProvider: config.ProviderCerebras, MemoirCerebrasAPIKey: "k", MemoirModel: "qwen-3.8-27b"}, false, "*llm.Client"},
		{"claude", config.Config{MemoirProvider: config.ProviderClaude, MemoirClaudeAPIKey: "k", MemoirModel: "claude-opus-5-5"}, false, "*llm.Claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg, nil)
			if (s.memoirLLM == nil) != tc.wantNil {
				t.Fatalf("memoirLLM nil = %v, want %v", s.memoirLLM == nil, tc.wantNil)
			}
			if got := fmt.Sprintf("%T", s.memoirLLM); !tc.wantNil && got != tc.wantTyp {
				t.Errorf("memoirLLM is %s, want %s", got, tc.wantTyp)
			}
		})
	}
}

// openRouterTestRequest is the subset of the OpenRouter STT body the tests
// inspect. []byte unmarshals from base64, exactly as the real request encodes it.
type openRouterTestRequest struct {
	Model      string `json:"model"`
	InputAudio struct {
		Data   []byte `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
	ResponseFormat         string   `json:"response_format"`
	TimestampGranularities []string `json:"timestamp_granularities"`
	Provider               struct {
		Options struct {
			Azure struct {
				EnhancedMode struct {
					ModelOptions struct {
						TranscribeStyle string `json:"transcribeStyle"`
					} `json:"modelOptions"`
				} `json:"enhancedMode"`
			} `json:"azure"`
		} `json:"options"`
	} `json:"provider"`
}

func readOpenRouterRequest(t *testing.T, r *http.Request) openRouterTestRequest {
	t.Helper()
	var req openRouterTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode OpenRouter body: %v", err)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer or-key" {
		t.Errorf("authorization %q, want Bearer or-key", got)
	}
	if r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content-type %q, want application/json", r.Header.Get("Content-Type"))
	}
	return req
}

func TestTranscribeProviderSelection(t *testing.T) {
	s := New(config.Config{
		TranscribeProviders: []string{config.ProviderAssemblyAI},
		TranscribeProvider:  config.ProviderAssemblyAI,
	}, nil)

	got := s.TranscribeProviders()
	if len(got) != 1 || got[0] != config.ProviderAssemblyAI {
		t.Fatalf("providers %v, want [%s]", got, config.ProviderAssemblyAI)
	}
	if got := s.TranscribeProvider(); got != config.ProviderAssemblyAI {
		t.Errorf("provider %q, want %q", got, config.ProviderAssemblyAI)
	}
	if !s.SetTranscribeProvider(config.ProviderAssemblyAI) {
		t.Error("could not select the configured provider")
	}
	if s.SetTranscribeProvider("not-configured") {
		t.Error("selected a provider that has no key")
	}
	if got := s.TranscribeProvider(); got != config.ProviderAssemblyAI {
		t.Errorf("provider %q after the rejected switch, want %q", got, config.ProviderAssemblyAI)
	}
}

func testWAV() []byte {
	const rate = 16000
	data := make([]byte, rate) // 0.25 s of silence, 16-bit mono
	b := make([]byte, 44+len(data))
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], uint32(36+len(data)))
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	binary.LittleEndian.PutUint16(b[20:22], 1)
	binary.LittleEndian.PutUint16(b[22:24], 1)
	binary.LittleEndian.PutUint32(b[24:28], rate)
	binary.LittleEndian.PutUint32(b[28:32], rate*2)
	binary.LittleEndian.PutUint16(b[32:34], 2)
	binary.LittleEndian.PutUint16(b[34:36], 16)
	copy(b[36:40], "data")
	binary.LittleEndian.PutUint32(b[40:44], uint32(len(data)))
	return b
}

func TestBatchTranscribeRouteIsGone(t *testing.T) {
	s := &Server{cfg: config.Config{AssemblyAIKey: "aai", TranscribeProvider: config.ProviderAssemblyAI}}
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe", strings.NewReader("x"))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "audio/webm")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestTranscribeStreamAssemblyAI(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	oldURL := assemblyAILiveURL
	defer func() { assemblyAILiveURL = oldURL }()

	var gotConfig, gotAudio string
	var parts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("MultipartReader: %v", err)
			return
		}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("NextPart: %v", err)
				return
			}
			b, _ := io.ReadAll(p)
			parts++
			switch p.FormName() {
			case "config":
				gotConfig = string(b)
			case "audio":
				gotAudio = string(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"streamed transcript","words":[
			{"text":"streamed","start":1200,"end":1500},
			{"text":"transcript","start":1510,"end":1900}]}`))
	}))
	defer srv.Close()
	assemblyAILiveURL = srv.URL

	s := &Server{
		cfg: config.Config{
			AssemblyAIKey:      "aai-key",
			TranscribeProvider: config.ProviderAssemblyAI,
		},
		httpShort: srv.Client(),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe/stream", bytes.NewReader(testWAV()))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "audio/wav")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text        string `json:"text"`
		FirstWordMS *int   `json:"first_word_ms"`
		LastWordMS  *int   `json:"last_word_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Text != "streamed transcript" {
		t.Fatalf("got %+v, err %v", out, err)
	}
	if out.FirstWordMS == nil || *out.FirstWordMS != 1200 {
		t.Errorf("first_word_ms = %v, want 1200", out.FirstWordMS)
	}
	if out.LastWordMS == nil || *out.LastWordMS != 1900 {
		t.Errorf("last_word_ms = %v, want 1900", out.LastWordMS)
	}
	if parts != 2 || !strings.Contains(gotConfig, `"sample_rate":16000`) ||
		!strings.Contains(gotConfig, `"timestamps":true`) || len(gotAudio) == 0 {
		t.Errorf("parts=%d config=%q audio=%d bytes", parts, gotConfig, len(gotAudio))
	}
}

// Exactly ?timestamps=0 or ?timestamps=false asks AssemblyAI for no word
// timings and drops first_word_ms/last_word_ms from the answer, even if the
// provider sends timings anyway. No query or any other value (other case,
// spaces, other words) is today's behaviour, which comart relies on.
func TestTranscribeStreamTimestampsQuery(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	oldURL := assemblyAILiveURL
	defer func() { assemblyAILiveURL = oldURL }()

	configs := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("MultipartReader: %v", err)
			return
		}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("NextPart: %v", err)
				return
			}
			b, _ := io.ReadAll(p)
			if p.FormName() == "config" {
				// Never block: a subtest that failed early must not hang the next.
				select {
				case configs <- string(b):
				default:
					t.Errorf("unread config part left over: %s", b)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"streamed transcript","words":[
			{"text":"streamed","start":1200,"end":1500},
			{"text":"transcript","start":1510,"end":1900}]}`))
	}))
	defer srv.Close()
	assemblyAILiveURL = srv.URL

	s := &Server{
		cfg: config.Config{
			AssemblyAIKey:      "aai-key",
			TranscribeProvider: config.ProviderAssemblyAI,
		},
		httpShort: srv.Client(),
	}
	const (
		withTimings    = `{"sample_rate":16000,"channels":1,"timestamps":true}`
		withoutTimings = `{"sample_rate":16000,"channels":1,"timestamps":false}`
	)
	for _, tc := range []struct {
		query      string
		wantConfig string
	}{
		{"", withTimings},
		{"?timestamps=0", withoutTimings},
		{"?timestamps=false", withoutTimings},
		{"?other=1&timestamps=0", withoutTimings},
		{"?timestamps=1", withTimings},
		{"?timestamps=true", withTimings},
		{"?timestamps=FALSE", withTimings},
		{"?timestamps=False", withTimings},
		{"?timestamps=%200", withTimings},
		{"?timestamps=00", withTimings},
		{"?timestamps=off", withTimings},
		{"?timestamps=no", withTimings},
		{"?timestamps=", withTimings},
		{"?timestamps", withTimings},
		{"?Timestamps=0", withTimings},
	} {
		name := tc.query
		if name == "" {
			name = "no query"
		}
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/transcribe/stream"+tc.query, bytes.NewReader(testWAV()))
			req.Host = "127.0.0.1:8770"
			req.Header.Set("Content-Type", "audio/wav")
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			// Take the config before any Fatal, so none is left for the next case.
			var gotConfig string
			select {
			case gotConfig = <-configs:
			default:
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if gotConfig == "" {
				t.Fatal("AssemblyAI received no config part")
			}
			if gotConfig != tc.wantConfig {
				t.Errorf("config part %s, want %s", gotConfig, tc.wantConfig)
			}

			var out map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("answer %q is not JSON: %v", rec.Body.String(), err)
			}
			if string(out["text"]) != `"streamed transcript"` {
				t.Errorf("text %s, want \"streamed transcript\"", out["text"])
			}
			first, hasFirst := out["first_word_ms"]
			last, hasLast := out["last_word_ms"]
			if tc.wantConfig == withoutTimings {
				if hasFirst || hasLast || len(out) != 1 {
					t.Errorf("answer %s, want text only", rec.Body.String())
				}
				return
			}
			if string(first) != "1200" || string(last) != "1900" || len(out) != 3 {
				t.Errorf("answer %s, want first_word_ms 1200 and last_word_ms 1900", rec.Body.String())
			}
		})
	}
}

func TestTranscribeWebSocket(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	oldURL := assemblyAILiveURL
	defer func() { assemblyAILiveURL = oldURL }()

	var gotAudio int
	var gotConfig string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("MultipartReader: %v", err)
			return
		}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("NextPart: %v", err)
				return
			}
			b, _ := io.ReadAll(p)
			switch p.FormName() {
			case "audio":
				gotAudio = len(b)
			case "config":
				gotConfig = string(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ws transcript","words":[
			{"text":"ws","start":300,"end":500},
			{"text":"transcript","start":520,"end":900}]}`))
	}))
	defer upstream.Close()
	assemblyAILiveURL = upstream.URL

	s := &Server{
		cfg: config.Config{
			AssemblyAIKey:      "aai",
			TranscribeProvider: config.ProviderAssemblyAI,
		},
		httpShort: upstream.Client(),
	}
	srv := httptest.NewUnstartedServer(s)
	srv.Start()
	defer srv.Close()
	// The handler forwards the streamed body to this same server over loopback,
	// exactly as it does in production.
	s.cfg.ListenAddr = strings.TrimPrefix(srv.URL, "http://")

	// The WS transport forwards without a query, so comart keeps its timings,
	// and a timestamps=0 on the WS URL itself is ignored, not passed on.
	for _, query := range []string{"?type=audio/wav", "?type=audio/wav&timestamps=0"} {
		t.Run(query, func(t *testing.T) {
			gotAudio, gotConfig = 0, ""
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/transcribe/ws" + query
			c, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.CloseNow()

			if err := c.Write(ctx, websocket.MessageBinary, testWAV()); err != nil {
				t.Fatalf("write audio: %v", err)
			}
			if err := c.Write(ctx, websocket.MessageText, []byte("stop")); err != nil {
				t.Fatalf("write stop: %v", err)
			}
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("read answer: %v", err)
			}
			var out map[string]json.RawMessage
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("answer %q is not JSON: %v", data, err)
			}
			if string(out["text"]) != `"ws transcript"` || string(out["first_word_ms"]) != "300" ||
				string(out["last_word_ms"]) != "900" || len(out) != 3 {
				t.Errorf("answer %s, want text, first_word_ms 300 and last_word_ms 900", data)
			}
			if gotAudio == 0 {
				t.Error("AssemblyAI received no PCM audio")
			}
			if want := `{"sample_rate":16000,"channels":1,"timestamps":true}`; gotConfig != want {
				t.Errorf("config part %s, want %s", gotConfig, want)
			}
		})
	}
}

func TestTranscribeStreamDemandsAudioType(t *testing.T) {
	s := &Server{cfg: config.Config{AssemblyAIKey: "aai"}}
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe/stream", strings.NewReader("{}"))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", rec.Code)
	}
}

func TestTranscribeVerbatimProvider(t *testing.T) {
	oldURL := openRouterSTTURL
	defer func() { openRouterSTTURL = oldURL }()

	var got openRouterTestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readOpenRouterRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"Hello world.","duration":1.0,` +
			`"segments":[{"id":0,"start":0.8,"end":1.0,"text":"world."},{"id":1,"start":0.1,"end":0.4,"text":"Hello"}],` +
			`"words":[{"word":"world.","start":0.8,"end":1.0},{"word":"Hello","start":0.1,"end":0.4}]}`))
	}))
	defer srv.Close()
	openRouterSTTURL = srv.URL

	s := &Server{
		cfg: config.Config{
			OpenRouterTranscribeAPIKey: "or-key",
		},
		httpOpenRouter: srv.Client(),
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("audio", "clip.mp3")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("audio-bytes"))
	_ = form.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/transcribe-verbatim", &body)
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Model != openRouterMAIModel || got.InputAudio.Format != "mp3" || string(got.InputAudio.Data) != "audio-bytes" {
		t.Errorf("got model=%q format=%q audio=%q", got.Model, got.InputAudio.Format, got.InputAudio.Data)
	}
	if got.ResponseFormat != "verbose_json" || len(got.TimestampGranularities) != 2 {
		t.Errorf("got response_format=%q granularities=%v", got.ResponseFormat, got.TimestampGranularities)
	}
	if got.Provider.Options.Azure.EnhancedMode.ModelOptions.TranscribeStyle != "verbatim" {
		t.Errorf("verbatim style missing from provider options: %+v", got.Provider)
	}

	var out struct {
		Lines []transcriptLine `json:"lines"`
		Words []transcriptWord `json:"words"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Lines) != 2 || out.Lines[0].Text != "Hello" || out.Lines[0].Start != 0.1 || out.Lines[1].Text != "world." {
		t.Errorf("lines %+v", out.Lines)
	}
	if len(out.Words) != 2 || out.Words[0].Text != "Hello" || out.Words[0].End != 0.4 {
		t.Errorf("words %+v", out.Words)
	}
}

func TestTranscribeVerbatimErrorKeepsClientCompatibility(t *testing.T) {
	oldURL := openRouterSTTURL
	defer func() { openRouterSTTURL = oldURL }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":400,"message":"bad audio"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	openRouterSTTURL = srv.URL

	s := &Server{
		cfg:            config.Config{OpenRouterTranscribeAPIKey: "or-key"},
		httpOpenRouter: srv.Client(),
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("audio", "clip.mp3")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("audio-bytes"))
	_ = form.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/transcribe-verbatim", &body)
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	var out struct {
		OpenRouterStatus int `json:"openRouterStatus"`
		AzureStatus      int `json:"azureStatus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.OpenRouterStatus != 400 || out.AzureStatus != 400 {
		t.Errorf("got %+v, want both status fields 400", out)
	}
}

// /api/ask attaches the stored screenshots in the order of their ids and tells
// the prompt how many there are. The client keeps only ids, never image bytes.
func TestAskCarriesScreenshotsInOrder(t *testing.T) {
	m := &captureModel{reply: "The first screenshot shows a chart."}
	shots := newScreenshotStore(time.Hour, 1<<20, 4)
	s := &Server{
		cfg:          config.Config{AskProvider: config.ProviderCerebras},
		askProviders: []string{config.ProviderCerebras},
		askLLMs: map[string]llm.Streamer{
			config.ProviderCerebras: testLLM(m),
		},
		shots: shots,
	}
	first, err := shots.put([]byte("first"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	second, err := shots.put([]byte("second"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"question":"What do the screenshots show?","screenshots":[%q,%q],"timeline":[{"kind":"text","phrase":"the chart"},{"kind":"screenshot","screenshot":%q},{"kind":"text","phrase":"the legend"},{"kind":"screenshot","screenshot":%q}]}`, first, second, first, second)
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(body))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "first screenshot") {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	promptReq, _ := m.body.Load().(string)
	for _, want := range []string{
		"# SCREENSHOTS",
		"The user attached 2 screenshot(s) to this message.",
		"data:image/png;base64,Zmlyc3Q=",
		"data:image/jpeg;base64,c2Vjb25k",
		"# CONTEXT TIMELINE",
		"1. Selected text:",
		"the chart",
		"2. Screenshot 1",
		"3. Selected text:",
		"the legend",
		"4. Screenshot 2",
	} {
		if !strings.Contains(promptReq, want) {
			t.Errorf("model request missing %q", want)
		}
	}
	if i, j := strings.Index(promptReq, "Zmlyc3Q="), strings.Index(promptReq, "c2Vjb25k"); i < 0 || j < 0 || i > j {
		t.Errorf("image order is wrong: %d > %d", i, j)
	}
}

// A screenshot id that has been released (or expired) is a clear in-stream
// error, not an empty answer and not a panic.
func TestAskReportsMissingScreenshot(t *testing.T) {
	m := &captureModel{reply: "should not run"}
	shots := newScreenshotStore(time.Hour, 1<<20, 4)
	s := &Server{
		cfg:          config.Config{AskProvider: config.ProviderCerebras},
		askProviders: []string{config.ProviderCerebras},
		askLLMs: map[string]llm.Streamer{
			config.ProviderCerebras: testLLM(m),
		},
		shots: shots,
	}
	id, err := shots.put([]byte("gone"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	shots.release([]string{id})
	body := fmt.Sprintf(`{"question":"What was in it?","screenshots":[%q]}`, id)
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(body))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "no longer available") {
		t.Fatalf("body %q", out)
	}
	if m.body.Load() != nil {
		t.Errorf("model was called with a missing screenshot")
	}
}

// The client refuses the 11th capture, and the server repeats the cap because
// a stale tab or a hand-written request can still send it. The model must not
// be called with an over-limit card.
func TestAskRefusesMoreThanTenScreenshots(t *testing.T) {
	m := &captureModel{reply: "should not run"}
	shots := newScreenshotStore(time.Hour, 8<<20, 20)
	s := &Server{
		cfg:          config.Config{AskProvider: config.ProviderCerebras},
		askProviders: []string{config.ProviderCerebras},
		askLLMs: map[string]llm.Streamer{
			config.ProviderCerebras: testLLM(m),
		},
		shots: shots,
	}
	ids := make([]string, 0, maxAskImages+1)
	for i := 0; i <= maxAskImages; i++ {
		id, err := shots.put([]byte{byte(i)}, "image/png")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	body := fmt.Sprintf(`{"question":"Too many?","screenshots":%s}`, mustJSON(t, ids))
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(body))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	out := rec.Body.String()
	if !strings.Contains(out, "at most 10 screenshots") {
		t.Fatalf("body %q", out)
	}
	if m.body.Load() != nil {
		t.Errorf("model was called with too many screenshots")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
