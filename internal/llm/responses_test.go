package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// rsse builds a Responses API stream: a keep-alive comment, one data line per
// event, then [DONE].
func rsse(events ...string) string {
	var b strings.Builder
	b.WriteString(": OPENROUTER PROCESSING\n\n")
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

const (
	rCompleted  = `{"type":"response.completed","response":{"status":"completed"}}`
	rMsgAdded   = `{"type":"response.output_item.added","item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`
	rSearchAdd  = `{"type":"response.output_item.added","item":{"id":"st_1","type":"openrouter:web_search","status":"in_progress"}}`
	rSearchDone = `{"type":"response.output_item.done","item":{"id":"st_1","type":"openrouter:web_search","status":"completed","action":{"type":"search","query":"q"}}}`
	rFetchAdd   = `{"type":"response.output_item.added","item":{"id":"st_2","type":"openrouter:web_fetch","status":"in_progress"}}`
	rFetchDone  = `{"type":"response.output_item.done","item":{"id":"st_2","type":"openrouter:web_fetch","status":"completed","url":"https://example.com","content":"page"}}`
	rReasoning  = `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"openrouter-server-tool.x"}}`
)

func rDelta(s string) string {
	b, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": s})
	return string(b)
}

func newTestResponses(ft *fakeTransport, params map[string]any) *Responses {
	if params == nil {
		params = map[string]any{"max_output_tokens": 10}
	}
	return NewResponses(Config{Provider: "Test", URL: "https://test.invalid/api/v1/responses", APIKey: "k", Model: "m",
		Params: params}, &http.Client{Transport: ft})
}

// recorder collects what a stream reports, in order: text as "t:…", search
// transitions as "search:on"/"search:off".
type recorder struct{ events []string }

func (r *recorder) text(s string) { r.events = append(r.events, "t:"+s) }
func (r *recorder) search(on bool) {
	if on {
		r.events = append(r.events, "search:on")
	} else {
		r.events = append(r.events, "search:off")
	}
}
func (r *recorder) String() string { return strings.Join(r.events, " | ") }

// A search then a fetch: each opens and closes the search state, and reasoning
// items in between say nothing.
func TestResponsesReportsServerToolsAsTheyRun(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{rsse(
		`{"type":"response.created","response":{"status":"in_progress"}}`,
		rSearchAdd, rSearchDone, rReasoning, rFetchAdd, rFetchDone, rReasoning,
		rMsgAdded, rDelta("The "), rDelta("answer."), rCompleted,
	)}}
	var rec recorder
	res := newTestResponses(ft, nil).StreamWithSearch(context.Background(), "q", rec.text, rec.search)
	if !res.OK {
		t.Fatalf("result %+v", res)
	}
	want := "search:on | search:off | search:on | search:off | t:The  | t:answer."
	if got := rec.String(); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
}

// Two server tools open at once keep one "searching" span.
func TestResponsesOverlappingServerToolsAreOneSpan(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{rsse(
		rSearchAdd, rFetchAdd, rSearchDone, rFetchDone, rMsgAdded, rDelta("ok"), rCompleted,
	)}}
	var rec recorder
	res := newTestResponses(ft, nil).StreamWithSearch(context.Background(), "q", rec.text, rec.search)
	if got, want := rec.String(), "search:on | search:off | t:ok"; !res.OK || got != want {
		t.Fatalf("got %s (%+v), want %s", got, res, want)
	}
}

// A stream cut while a search is open must not leave the card searching.
func TestResponsesClosesAnOpenSearchWhenTheStreamIsCut(t *testing.T) {
	cut := ": OPENROUTER PROCESSING\n\ndata: " + rSearchAdd + "\n\n"
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{cut}}
	var rec recorder
	res := newTestResponses(ft, nil).StreamWithSearch(context.Background(), "q", rec.text, rec.search)
	if res.Code != "stream-cut" {
		t.Fatalf("result %+v", res)
	}
	if got := rec.String(); got != "search:on | search:off" {
		t.Fatalf("events %s", got)
	}
}

// Answer text closes a search whose done event never came.
func TestResponsesTextClosesAnUnfinishedSearch(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{rsse(rSearchAdd, rMsgAdded, rDelta("ok"), rCompleted)}}
	var rec recorder
	res := newTestResponses(ft, nil).StreamWithSearch(context.Background(), "q", rec.text, rec.search)
	if got := rec.String(); !res.OK || got != "search:on | search:off | t:ok" {
		t.Fatalf("got %s (%+v)", got, res)
	}
}

// Text written before a search and the answer after it are two message items;
// the second starts a new paragraph instead of being glued to the first.
func TestResponsesSeparatesMessageItems(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{rsse(
		rMsgAdded, rDelta("Let me check."),
		rSearchAdd, rSearchDone,
		`{"type":"response.output_item.added","item":{"id":"msg_2","type":"message","role":"assistant","content":[]}}`,
		rDelta("[Go 1.27.4](https://go.dev) is out."), rCompleted,
	)}}
	var sb strings.Builder
	res := newTestResponses(ft, nil).Stream(context.Background(), "q", func(s string) { sb.WriteString(s) })
	if want := "Let me check.\n\n[Go 1.27.4](https://go.dev) is out."; !res.OK || sb.String() != want {
		t.Fatalf("got %q (%+v), want %q", sb.String(), res, want)
	}
}

// A message that follows text ending in a newline gets no extra blank line,
// and the first message gets no leading separator.
func TestResponsesSeparatorOnlyWhenNeeded(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{rsse(
		rMsgAdded, rDelta("Line one\n"),
		`{"type":"response.output_item.added","item":{"id":"msg_2","type":"message","role":"assistant","content":[]}}`,
		rDelta("Line two"), rCompleted,
	)}}
	var sb strings.Builder
	res := newTestResponses(ft, nil).Stream(context.Background(), "q", func(s string) { sb.WriteString(s) })
	if want := "Line one\nLine two"; !res.OK || sb.String() != want {
		t.Fatalf("got %q (%+v), want %q", sb.String(), res, want)
	}
}

func TestResponsesFailures(t *testing.T) {
	cases := []struct {
		name, body, code, msg string
	}{
		{"incomplete at the token limit",
			rsse(rMsgAdded, rDelta("partial"), `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`),
			"truncated", "length limit"},
		{"incomplete for another reason",
			rsse(rMsgAdded, rDelta("partial"), `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`),
			"upstream", "content_filter"},
		{"failed",
			rsse(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`),
			"upstream", "boom"},
		{"error event",
			rsse(`{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`),
			"upstream", "slow down"},
		{"cut before completion",
			"data: " + rMsgAdded + "\n\ndata: " + rDelta("partial") + "\n\n",
			"stream-cut", "cut off"},
		{"completed without text",
			rsse(rReasoning, rCompleted),
			"empty-stream", ""},
		{"a 200 that is really an error body",
			`{"error":{"message":"bad key"}}`,
			"upstream", "bad key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := &fakeTransport{statuses: []int{200}, bodies: []string{tc.body}}
			res := newTestResponses(ft, nil).Stream(context.Background(), "q", nil)
			if res.OK || res.Code != tc.code || !strings.Contains(res.Message, tc.msg) {
				t.Fatalf("result %+v, want code %q with %q", res, tc.code, tc.msg)
			}
		})
	}
}

func TestResponsesRetriesBusyUpstream(t *testing.T) {
	ft := &fakeTransport{statuses: []int{503, 200}, bodies: []string{`{"error":{"message":"busy"}}`, rsse(rMsgAdded, rDelta("ok"), rCompleted)}}
	var sb strings.Builder
	res := newTestResponses(ft, nil).Stream(context.Background(), "q", func(s string) { sb.WriteString(s) })
	if !res.OK || sb.String() != "ok" || ft.calls.Load() != 2 {
		t.Fatalf("got %q %+v after %d calls", sb.String(), res, ft.calls.Load())
	}
}

// The request carries input items (prompt, then images in order), the params
// as given, and no chat-completions fields.
func TestResponsesRequestShape(t *testing.T) {
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{rsse(rMsgAdded, rDelta("ok"), rCompleted)}}
	params := map[string]any{
		"max_output_tokens": 10,
		"reasoning":         map[string]any{"effort": "medium"},
		"tools":             []map[string]any{{"type": "openrouter:web_search"}},
	}
	images := []Image{{MediaType: "image/png", Data: "AAA"}, {MediaType: "image/jpeg", Data: "BBB"}}
	res := newTestResponses(ft, params).StreamImages(context.Background(), "look", images, nil)
	if !res.OK {
		t.Fatalf("result %+v", res)
	}
	sent := ft.sent[0]
	if sent["model"] != "m" || sent["stream"] != true || sent["max_output_tokens"] != float64(10) {
		t.Errorf("body %v", sent)
	}
	for _, k := range []string{"messages", "max_tokens"} {
		if _, ok := sent[k]; ok {
			t.Errorf("chat-completions field %q sent", k)
		}
	}
	input, _ := sent["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input %v", sent["input"])
	}
	msg, _ := input[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if msg["type"] != "message" || msg["role"] != "user" || len(content) != 3 {
		t.Fatalf("user message %v", msg)
	}
	text, _ := content[0].(map[string]any)
	img1, _ := content[1].(map[string]any)
	img2, _ := content[2].(map[string]any)
	if text["type"] != "input_text" || text["text"] != "look" {
		t.Errorf("text part %v", text)
	}
	if img1["type"] != "input_image" || img1["image_url"] != "data:image/png;base64,AAA" ||
		img2["image_url"] != "data:image/jpeg;base64,BBB" {
		t.Errorf("image parts %v %v", img1, img2)
	}
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools %v", sent["tools"])
	}
}

// The client-tool loop resends every output item of the turn (reasoning items
// carry the server tools' results) plus one function_call_output per call,
// and keeps reporting server tools on later rounds.
func TestResponsesRunsTheClientToolLoop(t *testing.T) {
	fc := `{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"search_images","arguments":"{\"query\":\"flag of Uruguay\"}"}}`
	ft := &fakeTransport{
		statuses: []int{200, 200},
		bodies: []string{
			rsse(rSearchAdd, rSearchDone, rReasoning, rMsgAdded, rDelta("Blue and white."),
				`{"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Blue and white."}]}}`,
				`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"search_images","arguments":""}}`,
				fc, rCompleted),
			rsse(rFetchAdd, rFetchDone,
				`{"type":"response.output_item.added","item":{"id":"msg_2","type":"message","role":"assistant","content":[]}}`,
				rDelta("Here it is."), rCompleted),
		},
	}
	params := map[string]any{"tools": []map[string]any{{"type": "openrouter:web_search"}}}
	tools := []ToolDef{{Name: "search_images", Description: "search", Schema: map[string]any{"type": "object"}}}
	var calls []string
	var rec recorder
	res := newTestResponses(ft, params).StreamWithTools(context.Background(), "flag?", nil, tools,
		func(ctx context.Context, name string, input json.RawMessage, includeImages bool) ToolResult {
			if includeImages {
				t.Error("Responses asked for image blocks it cannot send")
			}
			calls = append(calls, name+":"+string(input))
			return ToolResult{Text: "img_1: Flag of Uruguay"}
		}, rec.text, rec.search)
	if !res.OK {
		t.Fatalf("result %+v", res)
	}
	want := "search:on | search:off | t:Blue and white. | search:on | search:off | t:\n\n | t:Here it is."
	if got := rec.String(); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	if len(calls) != 1 || calls[0] != `search_images:{"query":"flag of Uruguay"}` {
		t.Fatalf("executor calls %v", calls)
	}
	if len(ft.sent) != 2 {
		t.Fatalf("requests %d", len(ft.sent))
	}
	toolsSent, _ := ft.sent[0]["tools"].([]any)
	fn, _ := toolsSent[1].(map[string]any)
	if len(toolsSent) != 2 || fn["type"] != "function" || fn["name"] != "search_images" || fn["parameters"] == nil {
		t.Fatalf("tools %v", ft.sent[0]["tools"])
	}
	if _, nested := fn["function"]; nested {
		t.Errorf("function tool uses the chat-completions nesting: %v", fn)
	}
	input, _ := ft.sent[1]["input"].([]any)
	var types []string
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		types = append(types, item["type"].(string))
	}
	if got, want := strings.Join(types, ","), "message,openrouter:web_search,reasoning,message,function_call,function_call_output"; got != want {
		t.Fatalf("second input types\n got %s\nwant %s", got, want)
	}
	out, _ := input[len(input)-1].(map[string]any)
	if out["call_id"] != "call_1" || out["output"] != "img_1: Flag of Uruguay" {
		t.Errorf("function_call_output %v", out)
	}
	reasoning, _ := input[2].(map[string]any)
	if reasoning["encrypted_content"] != "openrouter-server-tool.x" {
		t.Errorf("reasoning item not resent verbatim: %v", reasoning)
	}
}

func TestResponsesStopsARunawayToolLoop(t *testing.T) {
	fc := `{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"show_images","arguments":"{}"}}`
	ft := &fakeTransport{statuses: []int{200}, bodies: []string{rsse(fc, rCompleted)}}
	tools := []ToolDef{{Name: "show_images", Schema: map[string]any{"type": "object"}}}
	res := newTestResponses(ft, nil).StreamWithTools(context.Background(), "q", nil, tools,
		func(context.Context, string, json.RawMessage, bool) ToolResult { return ToolResult{} }, nil, nil)
	if res.Code != "tool-loop" || int(ft.calls.Load()) != maxToolRounds+1 {
		t.Fatalf("result %+v after %d calls", res, ft.calls.Load())
	}
}

// The Responses client satisfies every interface the Ask handler checks for.
func TestResponsesImplementsTheAskInterfaces(t *testing.T) {
	var s Streamer = NewResponses(Config{}, nil)
	if _, ok := s.(ImageStreamer); !ok {
		t.Error("not an ImageStreamer")
	}
	if _, ok := s.(SearchStreamer); !ok {
		t.Error("not a SearchStreamer")
	}
	if _, ok := s.(SearchImageStreamer); !ok {
		t.Error("not a SearchImageStreamer")
	}
	if _, ok := s.(ToolStreamer); !ok {
		t.Error("not a ToolStreamer")
	}
}
