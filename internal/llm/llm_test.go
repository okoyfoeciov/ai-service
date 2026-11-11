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

// Images ride beside the prompt as image_url parts, in the order the caller
// supplied them. A text-only request keeps the old string content shape.
func TestStreamImagesUsesOrderedContentParts(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{sse(
		`{"choices":[{"delta":{"content":"seen"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)}}
	c := New(Config{Provider: "Test", URL: "https://test.invalid/v1/chat/completions", APIKey: "k", Model: "m"}, &http.Client{Transport: ft})
	var sb strings.Builder
	res := c.StreamImages(context.Background(), "what is this?", []Image{
		{MediaType: "image/png", Data: "AAA"},
		{MediaType: "image/jpeg", Data: "BBB"},
	}, func(s string) { sb.WriteString(s) })
	if !res.OK || sb.String() != "seen" {
		t.Fatalf("got %q %+v", sb.String(), res)
	}
	msgs, ok := ft.sent[0]["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages %v", ft.sent[0]["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("content %v", msg["content"])
	}
	text, _ := parts[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "what is this?" {
		t.Errorf("text part %v", text)
	}
	for i, want := range []string{"data:image/png;base64,AAA", "data:image/jpeg;base64,BBB"} {
		part, _ := parts[i+1].(map[string]any)
		img, _ := part["image_url"].(map[string]any)
		if part["type"] != "image_url" || img["url"] != want {
			t.Errorf("image part %d = %v, want %s", i+1, part, want)
		}
	}

	// No images: the text-only wire shape is unchanged.
	ft2 := &fakeTransport{statuses: []int{200}, bodies: []string{sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `[DONE]`)}}
	c2 := New(Config{Provider: "Test", URL: "https://test.invalid/v1/chat/completions", APIKey: "k", Model: "m"}, &http.Client{Transport: ft2})
	_ = c2.Stream(context.Background(), "plain", nil)
	msgs2, _ := ft2.sent[0]["messages"].([]any)
	msg2, _ := msgs2[0].(map[string]any)
	if msg2["content"] != "plain" {
		t.Errorf("text-only content = %v, want the plain string", msg2["content"])
	}
}

// Native client tools: the first turn requests search_images, the backend
// executes it, and the second turn answers with the tool result in the chat
// messages. This is the OpenAI-style loop OpenRouter uses.
func TestStreamWithToolsRunsTheNativeToolLoop(t *testing.T) {
	ft := &fakeTransport{
		statuses: []int{200, 200},
		bodies: []string{
			sse(
				`{"choices":[{"delta":{"content":"Let me look. "}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search_images","arguments":"{\"query\":\"map of Uruguay\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			),
			sse(`{"choices":[{"delta":{"content":"Here is the map."},"finish_reason":"stop"}]}`, `[DONE]`),
		},
	}
	c := New(Config{Provider: "Test", URL: "https://test.invalid/v1/chat/completions", APIKey: "k", Model: "m",
		Params: map[string]any{
			"max_tokens": 10,
			"tools":      []map[string]any{{"type": "openrouter:web_search"}},
		}}, &http.Client{Transport: ft})
	tools := []ToolDef{{
		Name: "search_images", Description: "search", Schema: map[string]any{
			"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required": []string{"query"},
		},
	}}
	var calls []string
	var sb strings.Builder
	res := c.StreamWithTools(context.Background(), "show me the map", nil, tools,
		func(ctx context.Context, name string, input json.RawMessage, includeImages bool) ToolResult {
			calls = append(calls, name+":"+string(input))
			return ToolResult{Text: `{"candidates":[{"id":"img_1"}]}`}
		}, func(s string) { sb.WriteString(s) }, nil)
	if !res.OK || sb.String() != "Let me look. Here is the map." {
		t.Fatalf("got %q %+v", sb.String(), res)
	}
	if len(calls) != 1 || calls[0] != `search_images:{"query":"map of Uruguay"}` {
		t.Fatalf("executor calls %v", calls)
	}
	if len(ft.sent) != 2 {
		t.Fatalf("requests %d", len(ft.sent))
	}
	toolsSent, _ := ft.sent[0]["tools"].([]any)
	if len(toolsSent) != 2 {
		t.Fatalf("first tools %v", ft.sent[0]["tools"])
	}
	serverTool, _ := toolsSent[0].(map[string]any)
	if serverTool["type"] != "openrouter:web_search" {
		t.Fatalf("server tool lost: %v", serverTool)
	}
	firstTool, _ := toolsSent[1].(map[string]any)
	fn, _ := firstTool["function"].(map[string]any)
	if firstTool["type"] != "function" || fn["name"] != "search_images" {
		t.Fatalf("tool %v", firstTool)
	}
	msgs, _ := ft.sent[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second messages %v", ft.sent[1]["messages"])
	}
	assistant, _ := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "Let me look. " {
		t.Errorf("assistant %v", assistant)
	}
	tcs, _ := assistant["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls %v", assistant["tool_calls"])
	}
	tool, _ := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" ||
		tool["content"] != `{"candidates":[{"id":"img_1"}]}` {
		t.Errorf("tool result %v", tool)
	}
}
