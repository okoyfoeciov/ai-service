package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// claudeSSE renders Messages API stream events: each is {event name, JSON}.
func claudeSSE(events ...[2]string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: " + e[0] + "\ndata: " + e[1] + "\n\n")
	}
	return b.String()
}

func evStart(model string) [2]string {
	return [2]string{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"` + model + `","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`}
}

func evText(s string) [2]string {
	b, _ := json.Marshal(s)
	return [2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":` + string(b) + `}}`}
}

func evThinking(s string) [2]string {
	b, _ := json.Marshal(s)
	return [2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":` + string(b) + `}}`}
}

func evStop(reason string) [2]string {
	return [2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"` + reason + `","stop_sequence":null},"usage":{"output_tokens":9}}`}
}

var evMessageStop = [2]string{"message_stop", `{"type":"message_stop"}`}

// claudeServer answers every request with the next (status, body) pair, the
// last repeating, and records what it received.
type claudeServer struct {
	*httptest.Server
	statuses []int
	bodies   []string
	header   http.Header
	calls    atomic.Int32
	req      *http.Request
	sent     map[string]any
}

func newClaudeServer(t *testing.T, statuses []int, bodies ...string) *claudeServer {
	t.Helper()
	cs := &claudeServer{statuses: statuses, bodies: bodies}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(cs.calls.Add(1)) - 1
		if i >= len(cs.statuses) {
			i = len(cs.statuses) - 1
		}
		cs.req = r
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &cs.sent)
		for k, v := range cs.header {
			w.Header()[k] = v
		}
		if cs.statuses[i] == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(cs.statuses[i])
		_, _ = io.WriteString(w, cs.bodies[i])
	}))
	t.Cleanup(cs.Close)
	return cs
}

func runClaude(t *testing.T, cs *claudeServer, effort string) (string, Result) {
	t.Helper()
	c := NewClaude(ClaudeConfig{APIKey: "k", BaseURL: cs.URL, Model: "claude-opus-5-5", MaxTokens: 100, Effort: effort}, cs.Client())
	var sb strings.Builder
	res := c.Stream(context.Background(), "explain pelican", func(s string) { sb.WriteString(s) })
	return sb.String(), res
}

func TestClaudeStreamsAnswerAndDropsThinking(t *testing.T) {
	cs := newClaudeServer(t, []int{200}, claudeSSE(
		evStart("claude-opus-5-5"), evThinking("hmm"), evText("A pelican "), evText("is a bird."), evStop("end_turn"), evMessageStop))
	text, res := runClaude(t, cs, "medium")
	if !res.OK || text != "A pelican is a bird." {
		t.Fatalf("got %q, %+v", text, res)
	}
	if cs.req.URL.Path != "/v1/messages" || cs.req.Method != http.MethodPost {
		t.Errorf("request %s %s", cs.req.Method, cs.req.URL.Path)
	}
	if got := cs.req.Header.Get("X-Api-Key"); got != "k" {
		t.Errorf("x-api-key %q", got)
	}
	if got := cs.req.Header.Get("Authorization"); got != "" {
		t.Errorf("unexpected Authorization %q", got)
	}
	if cs.sent["model"] != "claude-opus-5-5" || cs.sent["stream"] != true || cs.sent["max_tokens"] != float64(100) {
		t.Errorf("body %v", cs.sent)
	}
	if oc, _ := cs.sent["output_config"].(map[string]any); oc["effort"] != "medium" {
		t.Errorf("output_config %v", cs.sent["output_config"])
	}
	// Thinking is asked for in its adaptive form, never as budget_tokens or
	// disabled (Opus 5.5 rejects both).
	if th, _ := cs.sent["thinking"].(map[string]any); len(th) != 1 || th["type"] != "adaptive" {
		t.Errorf("thinking %v, want {type: adaptive}", cs.sent["thinking"])
	}
	for _, k := range []string{"tools", "tool_choice", "fallbacks", "temperature"} {
		if _, ok := cs.sent[k]; ok {
			t.Errorf("request carries %q", k)
		}
	}
	msgs, _ := cs.sent["messages"].([]any)
	if len(msgs) != 1 || !strings.Contains(string(mustJSON(msgs[0])), "explain pelican") {
		t.Errorf("messages %v", cs.sent["messages"])
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestClaudeOmitsEffortWhenUnset(t *testing.T) {
	cs := newClaudeServer(t, []int{200}, claudeSSE(evStart("m"), evText("x"), evStop("end_turn"), evMessageStop))
	if _, res := runClaude(t, cs, ""); !res.OK {
		t.Fatalf("%+v", res)
	}
	if _, ok := cs.sent["output_config"]; ok {
		t.Errorf("output_config sent: %v", cs.sent["output_config"])
	}
}

// The SDK reads ANTHROPIC_* from the environment by default. None of it may
// change where a request goes or who it says it is. The cases are separate
// because the SDK stops at the first credential it finds: with ANTHROPIC_API_KEY
// set it never looks at the token, the profile or the custom headers, so one
// test with every variable would pass even without WithoutEnvironmentDefaults.
func TestClaudeIgnoresAnthropicEnvironment(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"key and base url", map[string]string{
			"ANTHROPIC_BASE_URL": "http://127.0.0.1:1",
			"ANTHROPIC_API_KEY":  "env-key",
		}},
		{"auth token and base url", map[string]string{
			"ANTHROPIC_BASE_URL":   "http://127.0.0.1:1",
			"ANTHROPIC_AUTH_TOKEN": "env-token",
		}},
		{"custom headers", map[string]string{
			"ANTHROPIC_CUSTOM_HEADERS": "X-Evil: 1",
		}},
		{"profile", map[string]string{
			"ANTHROPIC_PROFILE": "nope",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_PROFILE"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cs := newClaudeServer(t, []int{200}, claudeSSE(evStart("m"), evText("x"), evStop("end_turn"), evMessageStop))
			if _, res := runClaude(t, cs, "medium"); !res.OK {
				t.Fatalf("%+v", res)
			}
			if got := cs.req.Header.Get("X-Api-Key"); got != "k" {
				t.Errorf("x-api-key %q, want the configured key", got)
			}
			if got := cs.req.Header.Get("Authorization"); got != "" {
				t.Errorf("Authorization %q leaked in from the environment", got)
			}
			if got := cs.req.Header.Get("X-Evil"); got != "" {
				t.Errorf("custom header %q leaked in from the environment", got)
			}
		})
	}
}

func TestClaudeStreamFailures(t *testing.T) {
	errEvent := [2]string{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}
	cases := []struct {
		name     string
		body     string
		wantCode string
		wantMsg  string
	}{
		{"cut before any stop", claudeSSE(evStart("m"), evText("part")), "stream-cut", "cut off"},
		{"max tokens", claudeSSE(evStart("m"), evText("part"), evStop("max_tokens"), evMessageStop), "truncated", "length limit"},
		{"context window", claudeSSE(evStart("m"), evText("part"), evStop("model_context_window_exceeded"), evMessageStop), "truncated", "too long"},
		{"refusal", claudeSSE(evStart("m"), evStop("refusal"), evMessageStop), "refusal", "declined"},
		{"empty answer", claudeSSE(evStart("m"), evThinking("only thoughts"), evStop("end_turn"), evMessageStop), "empty-stream", ""},
		{"error event in stream", claudeSSE(evStart("m"), evText("part"), errEvent), "upstream", "Overloaded"},
		{"unexpected tool use", claudeSSE(evStart("m"), evStop("tool_use"), evMessageStop), "upstream", "tool_use"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newClaudeServer(t, []int{200}, tc.body)
			_, res := runClaude(t, cs, "medium")
			if res.OK || res.Code != tc.wantCode || !strings.Contains(res.Message, tc.wantMsg) {
				t.Fatalf("got %+v, want code %q containing %q", res, tc.wantCode, tc.wantMsg)
			}
		})
	}
}

func TestClaudeKeepsAnswerFinishedBeforeCut(t *testing.T) {
	// A stop reason arrived, then the connection ended before message_stop.
	cs := newClaudeServer(t, []int{200}, claudeSSE(evStart("m"), evText("whole"), evStop("end_turn")))
	text, res := runClaude(t, cs, "medium")
	if !res.OK || text != "whole" {
		t.Fatalf("got %q, %+v", text, res)
	}
}

func TestClaudeRetriesBusyUpstream(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{0, 0}
	defer func() { retryDelays = old }()
	overloaded := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	cs := newClaudeServer(t, []int{529, 529, 200}, overloaded, overloaded,
		claudeSSE(evStart("m"), evText("ok"), evStop("end_turn"), evMessageStop))
	text, res := runClaude(t, cs, "medium")
	if !res.OK || text != "ok" || cs.calls.Load() != 3 {
		t.Fatalf("got %q, %+v after %d calls", text, res, cs.calls.Load())
	}

	cs = newClaudeServer(t, []int{529}, overloaded)
	_, res = runClaude(t, cs, "medium")
	if res.OK || res.Code != "http-529" || !strings.Contains(res.Message, "Overloaded") || cs.calls.Load() != 3 {
		t.Fatalf("got %+v after %d calls", res, cs.calls.Load())
	}
}

func TestClaudeDoesNotRetryClientErrorsOrLongRetryAfter(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{0, 0}
	defer func() { retryDelays = old }()

	cs := newClaudeServer(t, []int{400}, `{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`)
	_, res := runClaude(t, cs, "medium")
	if res.OK || res.Code != "http-400" || !strings.Contains(res.Message, "bad model") || cs.calls.Load() != 1 {
		t.Fatalf("got %+v after %d calls", res, cs.calls.Load())
	}

	cs = newClaudeServer(t, []int{429}, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	cs.header = http.Header{"Retry-After": {"60"}}
	_, res = runClaude(t, cs, "medium")
	if res.OK || res.Code != "http-429" || cs.calls.Load() != 1 {
		t.Fatalf("got %+v after %d calls", res, cs.calls.Load())
	}
}

func TestClaudeUnreachableAndCancelled(t *testing.T) {
	cs := newClaudeServer(t, []int{200}, "")
	cs.Close()
	_, res := runClaude(t, cs, "medium")
	if res.OK || res.Code != "network" {
		t.Fatalf("got %+v", res)
	}

	cs = newClaudeServer(t, []int{200}, claudeSSE(evStart("m"), evText("x"), evStop("end_turn"), evMessageStop))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewClaude(ClaudeConfig{APIKey: "k", BaseURL: cs.URL, Model: "m", MaxTokens: 10}, cs.Client())
	if res := c.Stream(ctx, "p", nil); res.OK || res.Code != "cancelled" {
		t.Fatalf("got %+v", res)
	}
}

var _ Streamer = (*Claude)(nil)
var _ Streamer = (*Client)(nil)
