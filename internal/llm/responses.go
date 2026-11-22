package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// Responses streams from OpenRouter's Responses API (/api/v1/responses). The
// Ask route uses it instead of chat completions for one reason: it reports
// OpenRouter's own server tools as they run. Each openrouter:web_search or
// openrouter:web_fetch call opens with response.output_item.added and closes
// with response.output_item.done, so the card can say "Searching the web…"
// for exactly as long as a search or fetch runs. On chat completions the same
// tools leave only `: OPENROUTER PROCESSING` keep-alive comments, and OpenRouter
// sends those during a long silent reasoning pause too (measured 2026-11-22 on
// GPT-6.2 Sol: comments for 44s on a question that needed no search), so they
// cannot tell a search from thinking.
//
// The wire shape differs from chat completions:
//   - the request has `input` items instead of `messages`, `max_output_tokens`
//     instead of `max_tokens`, and flat function tools ({type, name,
//     description, parameters});
//   - every SSE `data:` line is an event object whose `type` names it
//     (response.output_text.delta, response.output_item.added, …), and the
//     stream ends with response.completed, response.incomplete or
//     response.failed;
//   - the API is stateless, so a client-tool round resends the whole input:
//     the user message, every output item of the turn so far (reasoning items
//     carry the server tools' results in encrypted form) and one
//     function_call_output per call.
//
// Config.Params are copied into every request as they are; they must not set
// model, input or stream. Reasoning items are never shown: callers want the
// answer only (`reasoning.exclude` is a chat-completions field the Responses
// API ignores).
type Responses struct {
	// base carries the config and HTTP client, and lends its send/retry path.
	// Its chat-completions methods are never called through a Responses.
	base *Client
}

// NewResponses builds a Responses client. cfg.URL is the Responses endpoint.
func NewResponses(cfg Config, hc *http.Client) *Responses {
	return &Responses{base: New(cfg, hc)}
}

// Stream sends prompt as one user message and streams the answer.
func (r *Responses) Stream(ctx context.Context, prompt string, onText func(string)) Result {
	return r.run(ctx, prompt, nil, nil, nil, onText, nil)
}

// StreamImages is Stream with images attached after the prompt, in order.
func (r *Responses) StreamImages(ctx context.Context, prompt string, images []Image, onText func(string)) Result {
	return r.run(ctx, prompt, images, nil, nil, onText, nil)
}

// StreamWithSearch is Stream that also reports server-side searches and
// fetches through onSearch (see SearchStreamer).
func (r *Responses) StreamWithSearch(ctx context.Context, prompt string, onText func(string), onSearch func(bool)) Result {
	return r.run(ctx, prompt, nil, nil, nil, onText, onSearch)
}

// StreamImagesWithSearch is StreamImages that also reports server-side
// searches and fetches.
func (r *Responses) StreamImagesWithSearch(ctx context.Context, prompt string, images []Image, onText func(string), onSearch func(bool)) Result {
	return r.run(ctx, prompt, images, nil, nil, onText, onSearch)
}

// StreamWithTools runs the client-tool loop (see ToolStreamer) and reports
// server-side searches and fetches through onSearch.
func (r *Responses) StreamWithTools(ctx context.Context, prompt string, images []Image, tools []ToolDef, exec ToolExecutor, onText func(string), onSearch func(bool)) Result {
	return r.run(ctx, prompt, images, tools, exec, onText, onSearch)
}

// isServerTool reports whether an output item type is a web tool the provider
// runs itself: OpenRouter's own (openrouter:web_search, openrouter:web_fetch,
// what ai-service configures) or a provider's native search (web_search_call),
// which OpenRouter may use when the engine is "auto".
func isServerTool(itemType string) bool {
	return itemType == "openrouter:web_search" || itemType == "openrouter:web_fetch" ||
		itemType == "web_search_call"
}

// searchState turns the server-tool items' open and close events into
// onSearch(true) when the first one opens and onSearch(false) when the last
// open one closes, so a search followed at once by a fetch does not flicker.
type searchState struct {
	onSearch func(bool)
	open     map[string]bool
}

func (s *searchState) start(id string) {
	if s.onSearch == nil {
		return
	}
	if s.open == nil {
		s.open = map[string]bool{}
	}
	if len(s.open) == 0 {
		s.onSearch(true)
	}
	s.open[id] = true
}

func (s *searchState) finish(id string) {
	if !s.open[id] {
		return
	}
	delete(s.open, id)
	if len(s.open) == 0 {
		s.onSearch(false)
	}
}

// clear closes every open item: answer text is arriving, or the stream ended
// (an item whose done event never came must not leave the card "searching").
func (s *searchState) clear() {
	if len(s.open) == 0 {
		return
	}
	s.open = nil
	s.onSearch(false)
}

// textJoin forwards answer text and separates message items. The model can
// write a sentence, search, then write the answer as a new message item; the
// two arrive with no whitespace between them ("…release page.[Go 1.27.4]…"
// was measured on chat completions), so a message whose first text follows
// earlier text that does not end in a newline starts a new paragraph.
type textJoin struct {
	onText   func(string)
	chars    int
	lastByte byte
	newItem  bool // a message item opened and has not sent text yet
}

func (j *textJoin) messageStarted() { j.newItem = true }

func (j *textJoin) write(s string) {
	if s == "" {
		return
	}
	if j.newItem && j.chars > 0 && j.lastByte != '\n' {
		j.emit("\n\n")
	}
	j.newItem = false
	j.emit(s)
}

func (j *textJoin) emit(s string) {
	j.chars += len(s)
	j.lastByte = s[len(s)-1]
	if j.onText != nil {
		j.onText(s)
	}
}

// responsesCall is one function_call output item.
type responsesCall struct {
	CallID    string
	Name      string
	Arguments string
}

// responsesTurn is one streamed response: its finished output items (resent as
// input on the next client-tool round), its function calls and how it ended.
type responsesTurn struct {
	Items      []json.RawMessage
	Calls      []responsesCall
	Status     string // completed, incomplete or failed; "" if the stream was cut
	Incomplete string // incomplete_details.reason
	Failure    string
	Err        error
}

func (r *Responses) run(ctx context.Context, prompt string, images []Image, tools []ToolDef, exec ToolExecutor, onText func(string), onSearch func(bool)) Result {
	cfg := r.base.cfg
	input := []any{responsesUserMessage(prompt, images)}
	allTools := mergeResponsesTools(cfg.Params["tools"], tools)
	search := &searchState{onSearch: onSearch}
	defer search.clear()
	text := &textJoin{onText: onText}
	rounds := 0

	for {
		body := make(map[string]any, len(cfg.Params)+4)
		for k, v := range cfg.Params {
			body[k] = v
		}
		body["model"] = cfg.Model
		body["input"] = input
		body["stream"] = true
		if len(allTools) > 0 {
			body["tools"] = allTools
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return Result{Code: "encode", Message: err.Error()}
		}

		res, failure := r.base.postStream(ctx, payload)
		if res == nil {
			return failure
		}
		turn := r.readTurn(res.Body, text, search)
		_ = res.Body.Close()
		if ctx.Err() != nil {
			return Result{Code: "cancelled"}
		}
		if turn.Failure != "" {
			log.Printf("llm[%s]: error in stream: %s", cfg.Provider, turn.Failure)
			return Result{Code: "upstream", Message: cfg.Provider + " error: " + turn.Failure}
		}
		if turn.Status == "" {
			log.Printf("llm[%s]: stream cut after %d chars: %v", cfg.Provider, text.chars, turn.Err)
			return Result{Code: "stream-cut", Message: "The answer was cut off. Try again."}
		}
		if turn.Status == "incomplete" {
			if turn.Incomplete == "max_output_tokens" {
				log.Printf("llm[%s]: stopped at the token limit after %d chars", cfg.Provider, text.chars)
				return Result{Code: "truncated", Message: "The answer hit the length limit and was cut off. Try again."}
			}
			log.Printf("llm[%s]: incomplete (%s) after %d chars", cfg.Provider, turn.Incomplete, text.chars)
			return Result{Code: "upstream", Message: cfg.Provider + " stopped early (" + turn.Incomplete + "). Try again."}
		}

		if len(turn.Calls) > 0 {
			if exec == nil {
				return Result{Code: "upstream", Message: cfg.Provider + " requested a tool that is not configured."}
			}
			if rounds >= maxToolRounds {
				log.Printf("llm[%s]: stopped after %d tool rounds", cfg.Provider, rounds)
				return Result{Code: "tool-loop", Message: "The answer used too many tool calls. Try again."}
			}
			for _, item := range turn.Items {
				input = append(input, item)
			}
			for _, call := range turn.Calls {
				result := exec(ctx, call.Name, json.RawMessage(call.Arguments), false)
				output := result.Text
				if output == "" {
					output = "OK"
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": call.CallID,
					"output":  output,
				})
			}
			rounds++
			continue
		}

		if text.chars == 0 {
			log.Printf("llm[%s]: empty answer (status %q)", cfg.Provider, turn.Status)
			return Result{Code: "empty-stream"}
		}
		return Result{OK: true}
	}
}

// responsesEvent is the part of a Responses stream event this client reads.
type responsesEvent struct {
	Type  string          `json:"type"`
	Delta string          `json:"delta"`
	Item  json.RawMessage `json:"item"`
	// Response is set on response.completed, .incomplete and .failed.
	Response *struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error json.RawMessage `json:"error"`
	} `json:"response"`
	// Message, Code and Error are the `error` event's fields, and the shape of
	// a plain error object sent in place of an event.
	Message string          `json:"message"`
	Code    json.RawMessage `json:"code"`
	Error   json.RawMessage `json:"error"`
}

// responsesItem is the part of an output item this client reads.
type responsesItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// readTurn consumes one streamed response. Answer text goes to text, server
// tool items drive search, and finished items and function calls are kept for
// the next client-tool round.
func (r *Responses) readTurn(body io.Reader, text *textJoin, search *searchState) responsesTurn {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 16<<20) // a fetched page arrives in one line
	var (
		out     responsesTurn
		other   strings.Builder // non-SSE text: a 200 that is really an error body
		sawData bool
	)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			if !sawData && other.Len() < 64<<10 && !strings.HasPrefix(line, ":") &&
				!strings.HasPrefix(line, "event:") {
				other.WriteString(line)
				other.WriteByte('\n')
			}
			continue
		}
		sawData = true
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ev responsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			search.clear()
			text.write(ev.Delta)
		case "response.output_item.added":
			var item responsesItem
			_ = json.Unmarshal(ev.Item, &item)
			switch {
			case isServerTool(item.Type):
				search.start(item.ID)
			case item.Type == "message":
				text.messageStarted()
			}
		case "response.output_item.done":
			var item responsesItem
			_ = json.Unmarshal(ev.Item, &item)
			if isServerTool(item.Type) {
				search.finish(item.ID)
			}
			if item.Type == "function_call" {
				out.Calls = append(out.Calls, responsesCall{CallID: item.CallID, Name: item.Name, Arguments: item.Arguments})
			}
			if len(ev.Item) > 0 {
				out.Items = append(out.Items, append(json.RawMessage(nil), ev.Item...))
			}
		case "response.completed", "response.incomplete", "response.failed":
			out.Status = strings.TrimPrefix(ev.Type, "response.")
			if ev.Response != nil {
				if d := ev.Response.IncompleteDetails; d != nil {
					out.Incomplete = d.Reason
				}
				if ev.Type == "response.failed" && out.Failure == "" {
					out.Failure = "the response failed"
					if len(ev.Response.Error) > 0 && string(ev.Response.Error) != "null" {
						out.Failure = errMessage([]byte(`{"error":` + string(ev.Response.Error) + `}`))
					}
				}
			}
		case "error":
			if out.Failure == "" {
				out.Failure = responsesErrorText(ev)
			}
		case "":
			// Not an event: OpenAI-style {"error":…} sent mid-stream.
			if len(ev.Error) > 0 && string(ev.Error) != "null" && out.Failure == "" {
				out.Failure = errMessage([]byte(`{"error":` + string(ev.Error) + `}`))
			}
		}
	}
	out.Err = sc.Err()
	if !sawData && strings.TrimSpace(other.String()) != "" && out.Failure == "" {
		out.Failure = errMessage([]byte(other.String()))
	}
	return out
}

// responsesErrorText is an `error` event's message: its own message field, or
// a nested error object's, or its code.
func responsesErrorText(ev responsesEvent) string {
	if ev.Message != "" {
		return ev.Message
	}
	if len(ev.Error) > 0 && string(ev.Error) != "null" {
		if msg := errMessage([]byte(`{"error":` + string(ev.Error) + `}`)); msg != "no details" {
			return msg
		}
	}
	if code := strings.Trim(string(ev.Code), `"`); code != "" && code != "null" {
		return code
	}
	return "upstream error"
}

// responsesUserMessage is the user input item: the prompt, then the images in
// capture order.
func responsesUserMessage(prompt string, images []Image) map[string]any {
	content := make([]map[string]any, 0, len(images)+1)
	content = append(content, map[string]any{"type": "input_text", "text": prompt})
	for _, img := range images {
		content = append(content, map[string]any{
			"type":      "input_image",
			"image_url": "data:" + img.MediaType + ";base64," + img.Data,
			"detail":    "auto",
		})
	}
	return map[string]any{"type": "message", "role": "user", "content": content}
}

// mergeResponsesTools keeps the provider's server tools from Params and
// appends the client function tools in the Responses API's flat shape.
func mergeResponsesTools(existing any, defs []ToolDef) []any {
	var out []any
	switch v := existing.(type) {
	case []any:
		out = append(out, v...)
	case []map[string]any:
		for _, item := range v {
			out = append(out, item)
		}
	}
	for _, d := range defs {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        d.Name,
			"description": d.Description,
			"parameters":  d.Schema,
		})
	}
	return out
}
