package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	for _, path := range []string{"/api/transcribe", "/api/explain", "/api/memoir/check", "/api/memoir/paraphrase"} {
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

// The caps count what Cairn counts (JS String.length), not UTF-8 bytes. With a
// nil memoirLLM, a request that passes the length check stops at 503.
func TestMemoirCapsCountUTF16(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name, path, field, text string
		wantStatus              int
		wantError               string
	}{
		{"paraphrase 600 curly quotes", "/api/memoir/paraphrase", "text", strings.Repeat("’", 600), http.StatusServiceUnavailable, "Neither MEMOIR_CEREBRAS_API_KEY nor MEMOIR_CLAUDE_API_KEY is set."},
		{"paraphrase 601 curly quotes", "/api/memoir/paraphrase", "text", strings.Repeat("’", 601), http.StatusBadRequest, "selection too long (max 600 chars)"},
		{"paraphrase 300 emoji", "/api/memoir/paraphrase", "text", strings.Repeat("😀", 300), http.StatusServiceUnavailable, "Neither MEMOIR_CEREBRAS_API_KEY nor MEMOIR_CLAUDE_API_KEY is set."},
		{"paraphrase 301 emoji", "/api/memoir/paraphrase", "text", strings.Repeat("😀", 301), http.StatusBadRequest, "selection too long (max 600 chars)"},
		{"check 12000 curly quotes", "/api/memoir/check", "content", strings.Repeat("’", 12000), http.StatusServiceUnavailable, "Neither MEMOIR_CEREBRAS_API_KEY nor MEMOIR_CLAUDE_API_KEY is set."},
		{"check 12001 curly quotes", "/api/memoir/check", "content", strings.Repeat("’", 12001), http.StatusBadRequest, "entry too long (max 12000 chars)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, s, tc.path, map[string]string{tc.field: tc.text})
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

func TestMemoirParaphraseUnusableReply(t *testing.T) {
	cases := []struct {
		name, reply  string
		wantStatus   int
		wantError    string
		wantVariants int
	}{
		{"unparseable", "Sorry, I can't help with that.", http.StatusBadGateway, "The model returned nothing usable. Try again.", 0},
		{"deliberately empty", `{"variants":[]}`, http.StatusOK, "", 0},
		{"one variant", `{"variants":[{"label":"Plain","text":"I went home early."}]}`, http.StatusOK, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{memoirLLM: testLLM(fakeModel{reply: tc.reply})}
			rec := postJSON(t, s, "/api/memoir/paraphrase", map[string]string{"text": "I left for home early."})
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var body struct {
				Error    *string          `json:"error"`
				Warning  *string          `json:"warning"`
				Variants []map[string]any `json:"variants"`
				Raw      string           `json:"raw"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Raw != tc.reply {
				t.Errorf("raw %q, want %q", body.Raw, tc.reply)
			}
			if tc.wantError != "" {
				if body.Error == nil || *body.Error != tc.wantError {
					t.Fatalf("error %v, want %q", body.Error, tc.wantError)
				}
				return
			}
			if body.Error != nil || body.Warning != nil {
				t.Errorf("unexpected error %v / warning %v", body.Error, body.Warning)
			}
			if body.Variants == nil || len(body.Variants) != tc.wantVariants {
				t.Errorf("variants %v, want %d", body.Variants, tc.wantVariants)
			}
		})
	}
}

// explainServer builds a Server whose model client fails fast without the
// network and whose MW client answers from cache.
func explainServer(t *testing.T, cache mw.Cache) *Server {
	t.Helper()
	return &Server{explainLLM: testLLM(failingTransport{}), mw: mw.New(cache)}
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
		{"/api/memoir/paraphrase", "multipart/form-data; boundary=x"},
		{"/api/transcribe", "text/plain"},
		{"/api/transcribe", ""},
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
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe", strings.NewReader("x"))
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

func postAudio(t *testing.T, s *Server, contentType string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe", strings.NewReader(body))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestTranscribeUnconfiguredAndShortClip(t *testing.T) {
	s := &Server{}
	rec := postAudio(t, s, "audio/webm", strings.Repeat("a", 1500))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if errMsg := decodeError(t, rec); !strings.Contains(errMsg, "GROQ_API_KEY") || !strings.Contains(errMsg, "PARAKEET_URL") || !strings.Contains(errMsg, "MAI_TRANSCRIBE_API_KEY") {
		t.Fatalf("error %q does not name all three variables", errMsg)
	}

	s = &Server{cfg: config.Config{TranscribeProvider: config.ProviderParakeet, ParakeetURL: "http://127.0.0.1:8790"}}
	rec = postAudio(t, s, "audio/webm", strings.Repeat("a", 500))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("short clip: status %d, want 422", rec.Code)
	}
}

func TestTranscribeGroqProvider(t *testing.T) {
	var gotAuth, gotModel, gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = r.ParseMultipartForm(4 << 20)
		gotModel = r.FormValue("model")
		if f, hdr, err := r.FormFile("file"); err == nil {
			defer f.Close()
			b, _ := io.ReadAll(f)
			gotFile = hdr.Filename + ":" + string(b[:4])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"  hello from groq  "}`))
	}))
	defer srv.Close()

	oldURL := groqURL
	groqURL = srv.URL
	defer func() { groqURL = oldURL }()

	s := &Server{
		cfg: config.Config{
			TranscribeProvider: config.ProviderGroq,
			GroqAPIKey:         "g-secret",
			GroqModel:          "whisper-large-v3-turbo",
		},
		httpShort: srv.Client(),
	}
	rec := postAudio(t, s, "audio/mp4", strings.Repeat("abcd", 400))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Text != "hello from groq" {
		t.Fatalf("got %+v, err %v", out, err)
	}
	if gotAuth != "Bearer g-secret" || gotModel != "whisper-large-v3-turbo" || gotFile != "audio.mp4:abcd" {
		t.Errorf("upstream got auth=%q model=%q file=%q", gotAuth, gotModel, gotFile)
	}
}

func TestTranscribeParakeetProvider(t *testing.T) {
	oldInterval := parakeetPollInterval
	parakeetPollInterval = 5 * time.Millisecond
	defer func() { parakeetPollInterval = oldInterval }()

	t.Run("polls until done and joins lines", func(t *testing.T) {
		var polls atomic.Int32
		var gotFile string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
				_ = r.ParseMultipartForm(4 << 20)
				if f, hdr, err := r.FormFile("file"); err == nil {
					defer f.Close()
					b, _ := io.ReadAll(f)
					gotFile = hdr.Filename + ":" + fmt.Sprint(len(b))
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"job_id":"job123","sha256":"abc"}`))
			case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job123":
				switch polls.Add(1) {
				case 1:
					_, _ = w.Write([]byte(`{"job_id":"job123","status":"queued","result":null,"error":null}`))
				case 2:
					_, _ = w.Write([]byte(`{"job_id":"job123","status":"processing","result":null,"error":null}`))
				default:
					_, _ = w.Write([]byte(`{"job_id":"job123","status":"done","result":{"lines":[{"text":"First sentence.","start":0.1},{"text":"Second sentence.","start":1.5}],"words":[]},"error":null}`))
				}
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		s := &Server{
			cfg: config.Config{
				TranscribeProvider: config.ProviderParakeet,
				ParakeetURL:        srv.URL,
			},
			httpShort: srv.Client(),
		}
		rec := postAudio(t, s, "audio/webm;codecs=opus", strings.Repeat("x", 1500))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Text != "First sentence. Second sentence." {
			t.Fatalf("got %+v, err %v", out, err)
		}
		if gotFile != "audio.webm:1500" {
			t.Errorf("got file %q, want audio.webm:1500", gotFile)
		}
		if polls.Load() < 3 {
			t.Errorf("polls = %d, want >= 3", polls.Load())
		}
	})

	t.Run("job error becomes 502", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"job_id":"jerr"}`))
				return
			}
			_, _ = w.Write([]byte(`{"job_id":"jerr","status":"error","result":null,"error":"audio decode failed"}`))
		}))
		defer srv.Close()

		s := &Server{
			cfg:       config.Config{TranscribeProvider: config.ProviderParakeet, ParakeetURL: srv.URL},
			httpShort: srv.Client(),
		}
		rec := postAudio(t, s, "audio/webm", strings.Repeat("x", 1500))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", rec.Code)
		}
		if got := decodeError(t, rec); !strings.Contains(got, "audio decode failed") {
			t.Errorf("error %q", got)
		}
	})
}

func TestTranscribeMAIProvider(t *testing.T) {
	var gotKey, gotDef, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Ocp-Apim-Subscription-Key")
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		_ = r.ParseMultipartForm(4 << 20)
		gotDef = r.FormValue("definition")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"phrases":[{"text":"world.","offsetMilliseconds":800},{"text":"Hello","offsetMilliseconds":100}]}`))
	}))
	defer srv.Close()

	s := &Server{
		cfg: config.Config{
			TranscribeProvider:  config.ProviderMAITranscribe,
			MAITranscribeAPIKey: "mai-key",
			AzureSpeechEndpoint: srv.URL,
		},
		httpShort: srv.Client(),
	}
	rec := postAudio(t, s, "audio/webm", strings.Repeat("z", 1500))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Text != "Hello world." {
		t.Fatalf("got %+v, err %v", out, err)
	}
	if gotKey != "mai-key" || gotPath != "/speechtotext/transcriptions:transcribe?api-version=2025-10-15" || !strings.Contains(gotDef, `"MAI-Transcribe-2"`) {
		t.Errorf("got key=%q path=%q def=%q", gotKey, gotPath, gotDef)
	}
}
