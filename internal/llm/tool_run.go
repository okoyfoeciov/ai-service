package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
)

// maxToolRounds bounds the client-tool loop. The Ask handler has its own
// four-search cap; this is the outer backstop for a model that keeps calling
// show_images or another tool.
const maxToolRounds = 8

// remoteToolCall is one accumulated OpenAI-style tool call.
type remoteToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// turnResult is one chat-completions turn: the text it streamed, how it
// finished, and the tool calls it requested.
type turnResult struct {
	Text      string
	Finish    string
	Calls     []remoteToolCall
	Failure   string
	Done      bool
	Err       error
	TextChars int
}

// StreamWithTools is StreamImages with native client tools. The model decides
// whether to call them; this client executes each call through exec, appends
// the OpenAI-compatible assistant/tool messages, and continues the same turn.
func (c *Client) StreamWithTools(ctx context.Context, prompt string, images []Image, tools []ToolDef, exec ToolExecutor, onText func(string), onSearch func(bool)) Result {
	_ = onSearch // OpenRouter's server tools handle their own status.
	messages := []map[string]any{chatUserMessage(prompt, images)}
	allTools := mergeToolDefs(c.cfg.Params["tools"], tools)
	totalText := 0
	rounds := 0

	for {
		body := make(map[string]any, len(c.cfg.Params)+4)
		for k, v := range c.cfg.Params {
			body[k] = v
		}
		body["model"] = c.cfg.Model
		body["messages"] = messages
		body["stream"] = true
		if len(allTools) > 0 {
			body["tools"] = allTools
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return Result{Code: "encode", Message: err.Error()}
		}

		res, failure := c.postStream(ctx, payload)
		if res == nil {
			return failure
		}
		turn := c.readTurn(res.Body, onText)
		_ = res.Body.Close()
		if ctx.Err() != nil {
			return Result{Code: "cancelled"}
		}
		if turn.Failure != "" {
			log.Printf("llm[%s]: error in stream: %s", c.cfg.Provider, turn.Failure)
			return Result{Code: "upstream", Message: c.cfg.Provider + " error: " + turn.Failure}
		}
		totalText += turn.TextChars

		if len(turn.Calls) > 0 {
			if exec == nil {
				return Result{Code: "upstream", Message: c.cfg.Provider + " requested a tool that is not configured."}
			}
			if turn.Finish != "tool_calls" && !(turn.Finish == "" && turn.Done) {
				return Result{Code: "stream-cut", Message: "The answer was cut off. Try again."}
			}
			if rounds >= maxToolRounds {
				log.Printf("llm[%s]: stopped after %d tool rounds", c.cfg.Provider, rounds)
				return Result{Code: "tool-loop", Message: "The answer used too many tool calls. Try again."}
			}
			messages = append(messages, assistantToolMessage(turn))
			for _, call := range turn.Calls {
				result := exec(ctx, call.Name, json.RawMessage(call.Arguments), false)
				content := result.Text
				if content == "" {
					content = "OK"
				}
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": call.ID,
					"content":      content,
				})
			}
			rounds++
			continue
		}

		switch turn.Finish {
		case "length":
			log.Printf("llm[%s]: stopped at the token limit after %d chars", c.cfg.Provider, totalText)
			return Result{Code: "truncated", Message: "The answer hit the length limit and was cut off. Try again."}
		case "error":
			return Result{Code: "upstream", Message: c.cfg.Provider + " stopped with an error. Try again."}
		}
		if turn.Finish == "" && (turn.Err != nil || !turn.Done) {
			log.Printf("llm[%s]: stream cut after %d chars: %v", c.cfg.Provider, totalText, turn.Err)
			return Result{Code: "stream-cut", Message: "The answer was cut off. Try again."}
		}
		if totalText == 0 {
			log.Printf("llm[%s]: empty answer (finish %q)", c.cfg.Provider, turn.Finish)
			return Result{Code: "empty-stream"}
		}
		return Result{OK: true}
	}
}

func chatUserMessage(prompt string, images []Image) map[string]any {
	if len(images) == 0 {
		return map[string]any{"role": "user", "content": prompt}
	}
	parts := make([]map[string]any, 0, len(images)+1)
	parts = append(parts, map[string]any{"type": "text", "text": prompt})
	for _, img := range images {
		parts = append(parts, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + img.MediaType + ";base64," + img.Data,
			},
		})
	}
	return map[string]any{"role": "user", "content": parts}
}

func assistantToolMessage(turn turnResult) map[string]any {
	var content any
	if turn.Text != "" {
		content = turn.Text
	}
	calls := make([]map[string]any, 0, len(turn.Calls))
	for _, call := range turn.Calls {
		calls = append(calls, map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return map[string]any{
		"role":       "assistant",
		"content":    content,
		"tool_calls": calls,
	}
}

// mergeToolDefs keeps the provider's own server tools (OpenRouter's web search
// and web fetch) and appends the client function tools.
func mergeToolDefs(existing any, defs []ToolDef) []any {
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
			"type": "function",
			"function": map[string]any{
				"name":        d.Name,
				"description": d.Description,
				"parameters":  d.Schema,
			},
		})
	}
	return out
}

// postStream sends one payload, retrying a busy or broken upstream before any
// stream text arrives. It mirrors StreamImages' retry policy.
func (c *Client) postStream(ctx context.Context, payload []byte) (*http.Response, Result) {
	for attempt := 0; ; attempt++ {
		res, err := c.send(ctx, payload)
		if err != nil {
			if ctx.Err() != nil {
				return nil, Result{Code: "cancelled"}
			}
			return nil, Result{Code: "network", Message: c.cfg.Provider + " is unreachable (" + err.Error() + ")."}
		}
		if res.StatusCode == http.StatusOK {
			return res, Result{}
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		_ = res.Body.Close()
		if wait, retry := retryPlan(res.StatusCode, res.Header, attempt); retry {
			log.Printf("llm[%s]: HTTP %d, retrying: %s", c.cfg.Provider, res.StatusCode, truncate(string(raw), 200))
			if !sleep(ctx, wait) {
				return nil, Result{Code: "cancelled"}
			}
			continue
		}
		log.Printf("llm[%s]: HTTP %d: %s", c.cfg.Provider, res.StatusCode, truncate(string(raw), 300))
		return nil, Result{
			Code:    httpCode(res.StatusCode),
			Message: httpMessage(c.cfg.Provider, res.StatusCode, raw),
		}
	}
}

// readTurn consumes one streaming chat-completions response. Unlike read, it
// treats a tool-call finish as a valid pause and returns the accumulated calls
// for the outer loop.
func (c *Client) readTurn(body io.Reader, onText func(string)) turnResult {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var (
		out     turnResult
		other   strings.Builder
		seen    = map[int]*remoteToolCall{}
		sawData bool
	)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			if !sawData && other.Len() < 64<<10 && !strings.HasPrefix(line, ":") {
				other.WriteString(line)
				other.WriteByte('\n')
			}
			continue
		}
		sawData = true
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			out.Done = true
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		if msg := ch.errorText(); msg != "" && out.Failure == "" {
			out.Failure = msg
		}
		for _, choice := range ch.Choices {
			if choice.FinishReason != "" {
				out.Finish = choice.FinishReason
			}
			if choice.Delta.Content != "" {
				out.Text += choice.Delta.Content
				out.TextChars += len(choice.Delta.Content)
				if onText != nil {
					onText(choice.Delta.Content)
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				acc := seen[tc.Index]
				if acc == nil {
					acc = &remoteToolCall{}
					seen[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				acc.Arguments += tc.Function.Arguments
			}
		}
	}
	out.Err = sc.Err()
	if !sawData && strings.TrimSpace(other.String()) != "" && out.Failure == "" {
		out.Failure = errMessage([]byte(other.String()))
	}

	indices := make([]int, 0, len(seen))
	for i := range seen {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		call := seen[i]
		if call.ID == "" {
			call.ID = fmt.Sprintf("call_%d", i+1)
		}
		out.Calls = append(out.Calls, *call)
	}
	return out
}
