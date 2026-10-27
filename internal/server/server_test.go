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
	for _, path := range []string{"/api/transcribe/stream", "/api/transcribe-verbatim", "/api/explain", "/api/memoir/check"} {
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

// A web page can POST cross-site without a preflight only with text/plain or a
// form type, and a sandboxed iframe's Origin is "null", which the guard lets
// through. The billed routes therefore demand the type the clients really send.
func TestBilledRoutesDemandTheClientsContentType(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct{ path, ctype string }{
		{"/api/explain", "text/plain;charset=UTF-8"},
		{"/api/explain", ""},
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
		OpenRouterAPIKey:       "or",
		ClaudeAPIKey:           "cl",
		ExplainProviders:       []string{config.ProviderClaude, config.ProviderOpenRouter},
		ExplainProvider:        config.ProviderClaude,
		ExplainEffort:          "medium",
		ExplainReasoningTokens: 1024,
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
		{"cerebras", config.Config{MemoirProvider: config.ProviderCerebras, MemoirCerebrasAPIKey: "k", MemoirModel: "qwen-3.8-27b", MemoirReasoningEffort: "medium"}, false, "*llm.Client"},
		{"claude", config.Config{MemoirProvider: config.ProviderClaude, MemoirClaudeAPIKey: "k", MemoirModel: "claude-opus-5-5", MemoirEffort: "medium"}, false, "*llm.Claude"},
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
		_, _ = w.Write([]byte(`{"text":"streamed transcript"}`))
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
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Text != "streamed transcript" {
		t.Fatalf("got %+v, err %v", out, err)
	}
	if parts != 2 || !strings.Contains(gotConfig, `"sample_rate":16000`) || len(gotAudio) == 0 {
		t.Errorf("parts=%d config=%q audio=%d bytes", parts, gotConfig, len(gotAudio))
	}
}

func TestTranscribeWebSocket(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	oldURL := assemblyAILiveURL
	defer func() { assemblyAILiveURL = oldURL }()

	var gotAudio int
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
			if p.FormName() == "audio" {
				gotAudio = len(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ws transcript"}`))
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/transcribe/ws?type=audio/wav"
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
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("answer %q is not JSON: %v", data, err)
	}
	if out.Text != "ws transcript" {
		t.Errorf("text %q, want %q", out.Text, "ws transcript")
	}
	if gotAudio == 0 {
		t.Error("AssemblyAI received no PCM audio")
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
