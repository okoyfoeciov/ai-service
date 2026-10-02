// Package llm streams a chat completion from an OpenAI-compatible
// /chat/completions endpoint, calling OnText for every answer fragment as it
// arrives. ai-service runs two Clients: OpenRouter (Google Gemini 3.8 Flash)
// for /api/explain and Cerebras (Qwen 3.8 27B) for Memoir.
//
// Both providers speak the same streaming shape: SSE `data:` lines holding
// chunk objects, `: comment` keep-alives, and `data: [DONE]` at the end. They
// differ only in the request fields that set the token budget and reasoning,
// so those arrive as Config.Params rather than being hard-coded here.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config says where one Client sends its requests.
type Config struct {
	Provider string // name for logs and error messages ("OpenRouter", "Cerebras")
	URL      string // the provider's chat completions endpoint
	APIKey   string
	Model    string
	// Params are extra request fields: the provider's own names for the token
	// budget and the reasoning settings (see server.New). They must not set
	// model, messages or stream.
	Params map[string]any
}

// Result is how a completion ended. Code is a short machine tag
// ("http-503", "empty-stream", …); Message, when set, is fit to show a user.
type Result struct {
	OK      bool
	Code    string
	Message string
}

type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a Client. hc should have no overall timeout: an answer can stream
// for minutes, and the request context is what cancels it.
func New(cfg Config, hc *http.Client) *Client {
	return &Client{cfg: cfg, http: hc}
}

// retryDelays are the waits before each retry of a request that failed with a
// busy or broken upstream (429, 5xx) before any text arrived. A longer
// Retry-After than the wait is honoured up to maxRetryAfter; past that the
// quota will not be back in time, so the request fails at once.
var retryDelays = []time.Duration{time.Second, 2 * time.Second}

const maxRetryAfter = 5 * time.Second

// Stream sends prompt as one user message and calls onText for each fragment
// of the answer. Reasoning fragments are dropped: callers want the answer only.
func (c *Client) Stream(ctx context.Context, prompt string, onText func(string)) Result {
	body := map[string]any{}
	for k, v := range c.cfg.Params {
		body[k] = v
	}
	body["model"] = c.cfg.Model
	body["messages"] = []map[string]string{{"role": "user", "content": prompt}}
	body["stream"] = true
	payload, err := json.Marshal(body)
	if err != nil {
		return Result{Code: "encode", Message: err.Error()}
	}

	for attempt := 0; ; attempt++ {
		res, err := c.send(ctx, payload)
		if err != nil {
			if ctx.Err() != nil {
				return Result{Code: "cancelled"}
			}
			return Result{Code: "network", Message: c.cfg.Provider + " is unreachable (" + err.Error() + ")."}
		}
		if res.StatusCode == http.StatusOK {
			defer res.Body.Close()
			return c.read(ctx, res.Body, onText)
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		retryable := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
		wait := time.Duration(0)
		if attempt < len(retryDelays) {
			wait = retryDelays[attempt]
		}
		if secs, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil {
			if after := time.Duration(secs) * time.Second; after > maxRetryAfter {
				retryable = false
			} else if after > wait {
				wait = after
			}
		}
		if retryable && attempt < len(retryDelays) {
			log.Printf("llm[%s]: HTTP %d, retrying: %s", c.cfg.Provider, res.StatusCode, truncate(string(raw), 200))
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return Result{Code: "cancelled"}
			}
		}
		log.Printf("llm[%s]: HTTP %d: %s", c.cfg.Provider, res.StatusCode, truncate(string(raw), 300))
		return Result{
			Code:    fmt.Sprintf("http-%d", res.StatusCode),
			Message: fmt.Sprintf("%s error (HTTP %d): %s", c.cfg.Provider, res.StatusCode, errMessage(raw)),
		}
	}
}

func (c *Client) send(ctx context.Context, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	return c.http.Do(req)
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Error is OpenAI's {"error":{"message":…}} or a bare {"error":"…"};
	// Message and Type are the flat {"message":…,"type":…} shape Cerebras uses.
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
	Type    string          `json:"type"`
}

// errorText is the error a chunk carries, or "" for an ordinary chunk.
func (ch chunk) errorText() string {
	if len(ch.Error) > 0 && string(ch.Error) != "null" {
		if msg := errMessage([]byte(`{"error":` + string(ch.Error) + `}`)); msg != "no details" {
			return msg
		}
		return "upstream error"
	}
	if ch.Choices == nil && (ch.Message != "" || ch.Type != "") {
		if ch.Message != "" {
			return ch.Message
		}
		return ch.Type
	}
	return ""
}

// read consumes the SSE body. The answer is judged at the end: an error chunk,
// a broken connection, a token-limit stop or no text at all is a failure, even
// after some text went out, so no caller takes a partial answer for a whole one.
func (c *Client) read(ctx context.Context, body io.Reader, onText func(string)) Result {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var (
		textChars int
		finish    string
		failure   string
		done      bool
		sawData   bool
		other     strings.Builder // non-SSE text: a 200 that is really an error body
	)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			// blank separators, `: keep-alive` comments, event: lines — or,
			// when no data line ever comes, a plain JSON error body.
			if !sawData && other.Len() < 64<<10 && !strings.HasPrefix(line, ":") {
				other.WriteString(line)
				other.WriteByte('\n')
			}
			continue
		}
		sawData = true
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		if msg := ch.errorText(); msg != "" && failure == "" {
			failure = msg
		}
		for _, choice := range ch.Choices {
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
			if choice.Delta.Content == "" {
				continue
			}
			textChars += len(choice.Delta.Content)
			if onText != nil {
				onText(choice.Delta.Content)
			}
		}
	}
	if ctx.Err() != nil {
		return Result{Code: "cancelled"} // the caller went away; not a provider failure
	}
	if failure != "" {
		log.Printf("llm[%s]: error in stream: %s", c.cfg.Provider, failure)
		return Result{Code: "upstream", Message: c.cfg.Provider + " error: " + failure}
	}
	if !sawData && strings.TrimSpace(other.String()) != "" {
		msg := errMessage([]byte(other.String()))
		log.Printf("llm[%s]: 200 without a stream: %s", c.cfg.Provider, msg)
		return Result{Code: "upstream", Message: c.cfg.Provider + " error: " + msg}
	}
	// The answer is whole once a finish_reason has arrived, even if the
	// connection then breaks before [DONE] or the trailing usage chunk.
	if err := sc.Err(); finish == "" && (err != nil || !done) {
		log.Printf("llm[%s]: stream cut after %d chars: %v", c.cfg.Provider, textChars, err)
		return Result{Code: "stream-cut", Message: "The answer was cut off. Try again."}
	}
	if finish == "length" {
		log.Printf("llm[%s]: stopped at the token limit after %d chars", c.cfg.Provider, textChars)
		return Result{Code: "truncated", Message: "The answer hit the length limit and was cut off. Try again."}
	}
	if finish == "error" {
		return Result{Code: "upstream", Message: c.cfg.Provider + " stopped with an error. Try again."}
	}
	if textChars == 0 {
		log.Printf("llm[%s]: empty answer (finish %q)", c.cfg.Provider, finish)
		return Result{Code: "empty-stream"}
	}
	return Result{OK: true}
}

// errMessage pulls a readable message out of an error body: OpenAI-style
// {"error":{"message":…}}, Cerebras' flat {"message":…}, or the raw text.
func errMessage(raw []byte) string {
	var j struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &j) == nil {
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(j.Error, &nested) == nil && nested.Message != "" {
			return truncate(nested.Message, 300)
		}
		var flat string
		if json.Unmarshal(j.Error, &flat) == nil && flat != "" {
			return truncate(flat, 300)
		}
		if j.Message != "" {
			return truncate(j.Message, 300)
		}
	}
	if s := strings.TrimSpace(string(raw)); s != "" {
		return truncate(s, 300)
	}
	return "no details"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
