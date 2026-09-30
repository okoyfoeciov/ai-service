package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTransport answers every request with the next status/body pair (the
// last pair repeats) and records the request bodies it saw.
type fakeTransport struct {
	statuses []int
	bodies   []string
	header   http.Header
	calls    atomic.Int32
	sent     []map[string]any
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	i := int(f.calls.Add(1)) - 1
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	var sent map[string]any
	b, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(b, &sent)
	f.sent = append(f.sent, sent)
	return &http.Response{
		StatusCode: f.statuses[i],
		Header:     f.header,
		Body:       io.NopCloser(strings.NewReader(f.bodies[i])),
		Request:    req,
	}, nil
}

func sse(chunks ...string) string {
	var b strings.Builder
	b.WriteString(": keep-alive\n\n")
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String()
}

func run(t *testing.T, ft *fakeTransport) (string, Result) {
	t.Helper()
	c := New(Config{Provider: "Test", URL: "https://test.invalid/v1/chat/completions", APIKey: "k", Model: "m",
		Params: map[string]any{"max_tokens": 10}}, &http.Client{Transport: ft})
	var sb strings.Builder
	res := c.Stream(context.Background(), "hi", func(s string) { sb.WriteString(s) })
	return sb.String(), res
}

func TestStreamCollectsAnswerAndDropsReasoning(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{sse(
		`{"choices":[{"delta":{"reasoning":"thinking…"}}]}`,
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":", world"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)}}
	text, res := run(t, ft)
	if !res.OK || text != "Hello, world" {
		t.Fatalf("got %q %+v", text, res)
	}
	sent := ft.sent[0]
	if sent["model"] != "m" || sent["stream"] != true || sent["max_tokens"] != float64(10) {
		t.Errorf("request body %v", sent)
	}
}

func TestStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
	}{
		{"error chunk", sse(`{"choices":[{"delta":{"content":"Hi"}}]}`, `{"error":{"message":"overloaded"},"choices":[{"delta":{},"finish_reason":"error"}]}`, `[DONE]`), "upstream"},
		{"token limit", sse(`{"choices":[{"delta":{"content":"{\"variants\":["},"finish_reason":"length"}]}`, `[DONE]`), "truncated"},
		{"cut off", sse(`{"choices":[{"delta":{"content":"Hi"}}]}`), "stream-cut"},
		{"empty", sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`), "empty-stream"},
		{"flat error chunk", sse(`{"choices":[{"delta":{"content":"Hi"}}]}`, `{"message":"Too many tokens","type":"too_many_tokens_error","code":"429"}`, `[DONE]`), "upstream"},
		{"string error chunk", sse(`{"choices":[{"delta":{"content":"Hi"}}]}`, `{"error":"overloaded"}`, `[DONE]`), "upstream"},
		{"200 with a JSON error body", `{"error":{"message":"model not found"}}`, "upstream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, res := run(t, &fakeTransport{statuses: []int{200}, bodies: []string{tc.body}})
			if res.OK || res.Code != tc.code {
				t.Fatalf("got %+v, want code %s", res, tc.code)
			}
		})
	}
}

// A finish_reason makes the answer whole even if the connection then breaks.
func TestStreamKeepsAnswerFinishedBeforeCut(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\n"}}
	text, res := run(t, ft)
	if !res.OK || text != "Hi" {
		t.Fatalf("got %q %+v", text, res)
	}
}

func TestStreamRetriesBusyUpstream(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{0, 0}
	defer func() { retryDelays = old }()

	ft := &fakeTransport{
		statuses: []int{503, 429, 200},
		bodies:   []string{`{"error":{"message":"busy"}}`, `{"message":"slow down"}`, sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `[DONE]`)},
	}
	text, res := run(t, ft)
	if !res.OK || text != "ok" || ft.calls.Load() != 3 {
		t.Fatalf("got %q %+v after %d calls", text, res, ft.calls.Load())
	}

	ft = &fakeTransport{statuses: []int{503}, bodies: []string{`{"error":{"message":"busy"}}`}}
	_, res = run(t, ft)
	if res.Code != "http-503" || !strings.Contains(res.Message, "busy") || ft.calls.Load() != 3 {
		t.Fatalf("got %+v after %d calls", res, ft.calls.Load())
	}
}

func TestStreamDoesNotRetryPastALongRetryAfter(t *testing.T) {
	ft := &fakeTransport{statuses: []int{429}, bodies: []string{`{"message":"quota"}`}, header: http.Header{"Retry-After": {"30"}}}
	_, res := run(t, ft)
	if res.Code != "http-429" || ft.calls.Load() != 1 {
		t.Fatalf("got %+v after %d calls", res, ft.calls.Load())
	}
}

func TestStreamDoesNotRetryClientErrors(t *testing.T) {
	ft := &fakeTransport{statuses: []int{401}, bodies: []string{`{"message":"Wrong API Key"}`}}
	_, res := run(t, ft)
	if res.Code != "http-401" || !strings.Contains(res.Message, "Wrong API Key") || ft.calls.Load() != 1 {
		t.Fatalf("got %+v after %d calls", res, ft.calls.Load())
	}
}
