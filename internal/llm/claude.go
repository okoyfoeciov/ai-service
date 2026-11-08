package llm

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ClaudeProvider names the Claude Platform in logs and error messages.
const ClaudeProvider = "Claude"

// ClaudeConfig says where one Claude client sends its requests.
type ClaudeConfig struct {
	APIKey  string
	BaseURL string // the API root, https://api.anthropic.com
	Model   string // a Claude model id, sent as given
	// MaxTokens bounds one answer, thinking included.
	MaxTokens int64
	// Effort is output_config.effort (low … max), how much the model thinks.
	// Empty leaves the model's own default, which differs between models.
	Effort string
	// Route says which route this client serves ("explain", "ask", "memoir"),
	// for the log only: Claude clients with the same model would otherwise log
	// identical lines.
	Route string
	// WebSearch enables Claude's server-side web search tool. Only /api/ask's
	// web-enabled choices set it; Explain and Memoir must not.
	WebSearch bool
	// WebSearchMaxUses caps searches in one turn when WebSearch is on.
	WebSearchMaxUses int64
	// WebFetch enables Claude's server-side web fetch tool. Like WebSearch, it
	// belongs only to /api/ask's web-enabled choices.
	WebFetch bool
	// WebFetchMaxUses caps fetches in one turn when WebFetch is on.
	WebFetchMaxUses int64
	// WebFetchMaxContentTokens caps the fetched text added to the context.
	// Zero leaves the API's own behaviour (no cap), so callers should set it.
	WebFetchMaxContentTokens int64
}

// Claude streams an answer from the Claude Platform's Messages API with the
// official SDK. It has the same Stream contract as Client, so a handler can
// use either.
//
// The request carries the model, max_tokens, adaptive thinking and the effort,
// plus Claude's web-search and web-fetch tools only on the web-enabled
// /api/ask choice. It carries no sampling settings and no refusal fallback. A
// fallback would answer from another model after a refusal, which is the silent
// re-routing this service is set up to avoid; a refusal is reported as an error
// instead.
// Adaptive thinking is asked for explicitly because leaving the field
// out means "no thinking" on some older models (Opus 4.8, 4.7) and "thinking"
// on Opus 5.5; the explicit form is the same on every model that has it.
type Claude struct {
	cfg    ClaudeConfig
	client anthropic.Client
}

// logName is the provider name in log lines: "Claude/memoir" when Route is set.
func (c *Claude) logName() string {
	if c.cfg.Route == "" {
		return ClaudeProvider
	}
	return ClaudeProvider + "/" + c.cfg.Route
}

// NewClaude builds a Claude client. hc should have no overall timeout, like New's.
//
// The SDK would otherwise read ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN,
// ANTHROPIC_BASE_URL, ANTHROPIC_PROFILE and a profile file from the machine, any
// of which could send a request to a different place or with a different
// identity than ai-service.env says. WithoutEnvironmentDefaults switches all of
// that off; the key and URL come only from cfg. SDK retries are off too: Stream
// retries on its own schedule, the same as Client.
func NewClaude(cfg ClaudeConfig, hc *http.Client) *Claude {
	return &Claude{cfg: cfg, client: anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(cfg.BaseURL),
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(hc),
		option.WithMaxRetries(0),
	)}
}

// Stream sends prompt as one user message and calls onText for each fragment
// of the answer. Thinking is dropped: callers want the answer only.
func (c *Claude) Stream(ctx context.Context, prompt string, onText func(string)) Result {
	return c.StreamImagesWithSearch(ctx, prompt, nil, onText, nil)
}

// StreamImages is Stream with images attached to the same user message. The
// images keep their slice order, which is the order the reader captured them;
// the prompt says how to read that order.
func (c *Claude) StreamImages(ctx context.Context, prompt string, images []Image, onText func(string)) Result {
	return c.StreamImagesWithSearch(ctx, prompt, images, onText, nil)
}

// StreamWithSearch is Stream with a server-tool web status callback.
func (c *Claude) StreamWithSearch(ctx context.Context, prompt string, onText func(string), onSearch func(bool)) Result {
	return c.StreamImagesWithSearch(ctx, prompt, nil, onText, onSearch)
}

// StreamImagesWithSearch is StreamImages with a web status callback. The
// callback fires when Claude opens a web_search or web_fetch server_tool_use
// block (onSearch(true)) and when the corresponding result starts arriving
// (onSearch(false)).
func (c *Claude) StreamImagesWithSearch(ctx context.Context, prompt string, images []Image, onText func(string), onSearch func(bool)) Result {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(images)+1)
	blocks = append(blocks, anthropic.NewTextBlock(prompt))
	for _, img := range images {
		blocks = append(blocks, anthropic.NewImageBlockBase64(img.MediaType, img.Data))
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.cfg.Model),
		MaxTokens: c.cfg.MaxTokens,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)},
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
	}
	if c.cfg.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(c.cfg.Effort)}
	}
	var tools []anthropic.ToolUnionParam
	if c.cfg.WebSearch {
		uses := c.cfg.WebSearchMaxUses
		if uses <= 0 {
			uses = 2
		}
		tools = append(tools, anthropic.ToolUnionParam{
			OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
				MaxUses:        anthropic.Int(uses),
				AllowedCallers: []string{"direct"},
			},
		})
	}
	if c.cfg.WebFetch {
		uses := c.cfg.WebFetchMaxUses
		if uses <= 0 {
			uses = 2
		}
		fetch := &anthropic.WebFetchTool20250910Param{
			MaxUses: anthropic.Int(uses),
		}
		if c.cfg.WebFetchMaxContentTokens > 0 {
			fetch.MaxContentTokens = anthropic.Int(c.cfg.WebFetchMaxContentTokens)
		}
		tools = append(tools, anthropic.ToolUnionParam{OfWebFetchTool20250910: fetch})
	}
	if len(tools) > 0 {
		params.Tools = tools
	}

	// A paused search or fetch turn is continued by appending the assistant
	// message it returned and asking again with the same tools (Messages API
	// pause_turn). The cap is the same kind of backstop as the HTTP retry
	// budget: it stops a provider loop from running forever.
	messages := params.Messages
	pauses := 0
	attempt := 0
	totalText := 0
	web := &webToolTracker{notify: onSearch}
	textOut := onText
	if onText != nil {
		textOut = func(s string) {
			totalText += len(s)
			onText(s)
		}
	}
	for {
		params.Messages = messages
		stream := c.client.Messages.NewStreaming(ctx, params)
		res, msg, httpErr := c.read(ctx, stream, textOut, web, totalText)
		_ = stream.Close()
		if httpErr == nil {
			if res.Code == "pause-turn" {
				if msg == nil || pauses >= maxPauseTurns {
					log.Printf("llm[%s]: paused %d times without finishing", c.logName(), pauses)
					return Result{Code: "stream-cut", Message: "The answer was cut off. Try again."}
				}
				messages = append(messages, msg.ToParam())
				pauses++
				continue
			}
			return res
		}

		raw := []byte(httpErr.RawJSON())
		wait, retry := retryPlan(httpErr.StatusCode, httpErr.Response.Header, attempt)
		if retry {
			log.Printf("llm[%s]: HTTP %d, retrying: %s", c.logName(), httpErr.StatusCode, truncate(string(raw), 200))
			attempt++
			if !sleep(ctx, wait) {
				return Result{Code: "cancelled"}
			}
			continue
		}
		log.Printf("llm[%s]: HTTP %d (request %s): %s", c.logName(), httpErr.StatusCode, httpErr.RequestID, truncate(string(raw), 300))
		return Result{
			Code:    httpCode(httpErr.StatusCode),
			Message: httpMessage(ClaudeProvider, httpErr.StatusCode, raw),
		}
	}
}

// maxPauseTurns caps how many times one answer may be resumed after a
// pause_turn, like the HTTP retry budget. A web search or fetch turn can pause
// more than once; a provider loop must not run forever.
const maxPauseTurns = 4

// webToolTracker keeps the web status correct when Claude runs more than one
// search or fetch in parallel: the answer is still working on the web until the
// last result arrives. It lives across pause_turn continuations.
type webToolTracker struct {
	pending map[string]bool
	active  bool
	notify  func(bool)
}

func (w *webToolTracker) start(id string) {
	if w == nil || w.notify == nil {
		return
	}
	if w.pending == nil {
		w.pending = make(map[string]bool)
	}
	w.pending[id] = true
	if !w.active {
		w.active = true
		w.notify(true)
	}
}

func (w *webToolTracker) finish(id string) {
	if w == nil || w.notify == nil {
		return
	}
	delete(w.pending, id)
	if w.active && len(w.pending) == 0 {
		w.active = false
		w.notify(false)
	}
}

// read consumes the stream. It returns a non-nil *anthropic.Error, and no
// Result, when the API answered the request itself with an HTTP error: Stream
// decides whether to retry that. Everything else, including an error event
// inside an open stream, is a Result. As in Client.read, the answer is judged at
// the end, so no caller takes a partial answer for a whole one.
//
// The accumulated Message is returned with the Result. StreamImages needs it
// only for pause_turn: the Messages API continues that turn when the paused
// assistant message is sent back unchanged.
func (c *Claude) read(ctx context.Context, stream interface {
	Next() bool
	Current() anthropic.MessageStreamEventUnion
	Err() error
}, onText func(string), web *webToolTracker, priorText int) (Result, *anthropic.Message, *anthropic.Error) {
	var (
		textChars int
		stop      anthropic.StopReason
		details   anthropic.RefusalStopDetails
		accErr    error
	)
	acc := &anthropic.Message{}
	for stream.Next() {
		ev := stream.Current()
		if err := acc.Accumulate(ev); err != nil && accErr == nil {
			accErr = err
		}
		switch ev := ev.AsAny().(type) {
		case anthropic.MessageStartEvent:
			// Say which model really answered, next to the one asked for: the
			// two lines in the log are the check that a request went where the
			// config says.
			log.Printf("llm[%s]: asked %s, answered by %s (message %s)", c.logName(), c.cfg.Model, ev.Message.Model, ev.Message.ID)
		case anthropic.ContentBlockStartEvent:
			switch {
			case ev.ContentBlock.Type == "server_tool_use" &&
				(ev.ContentBlock.Name == "web_search" || ev.ContentBlock.Name == "web_fetch"):
				web.start(ev.ContentBlock.ID)
			case ev.ContentBlock.Type == "web_search_tool_result",
				ev.ContentBlock.Type == "web_fetch_tool_result":
				web.finish(ev.ContentBlock.ToolUseID)
			}
		case anthropic.ContentBlockDeltaEvent:
			if td, ok := ev.Delta.AsAny().(anthropic.TextDelta); ok && td.Text != "" {
				textChars += len(td.Text)
				if onText != nil {
					onText(td.Text)
				}
			}
		case anthropic.MessageDeltaEvent:
			if ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
				details = ev.Delta.StopDetails
			}
		}
	}

	err := stream.Err()
	if ctx.Err() != nil {
		return Result{Code: "cancelled"}, nil, nil // the caller went away; not a provider failure
	}
	var apiErr *anthropic.Error
	isAPI := errors.As(err, &apiErr)
	if isAPI && apiErr.Response != nil && apiErr.StatusCode >= 400 {
		return Result{}, nil, apiErr // the request itself was refused
	}
	if err != nil && !isAPI && stop == "" && textChars == 0 {
		// Nothing arrived: the host was not reached, or the connection broke
		// before the first event.
		log.Printf("llm[%s]: request failed: %v", c.logName(), err)
		return Result{Code: "network", Message: ClaudeProvider + " is unreachable (" + err.Error() + ")."}, nil, nil
	}
	// The answer is whole once a stop reason has arrived, even if the connection
	// then breaks before message_stop.
	if stop == "" {
		if isAPI {
			msg := errMessage([]byte(apiErr.RawJSON()))
			log.Printf("llm[%s]: error in stream: %s", c.logName(), msg)
			return Result{Code: "upstream", Message: ClaudeProvider + " error: " + msg}, nil, nil
		}
		log.Printf("llm[%s]: stream cut after %d chars: %v", c.logName(), textChars, err)
		return Result{Code: "stream-cut", Message: "The answer was cut off. Try again."}, nil, nil
	}
	switch stop {
	case anthropic.StopReasonMaxTokens:
		log.Printf("llm[%s]: stopped at the token limit after %d chars", c.logName(), textChars)
		return Result{Code: "truncated", Message: "The answer hit the length limit and was cut off. Try again."}, nil, nil
	case anthropic.StopReasonModelContextWindowExceeded:
		log.Printf("llm[%s]: stopped at the context window limit after %d chars", c.logName(), textChars)
		return Result{Code: "truncated", Message: "The request was too long for the model."}, nil, nil
	case anthropic.StopReasonRefusal:
		log.Printf("llm[%s]: declined (category %q): %s", c.logName(), details.Category, details.Explanation)
		return Result{Code: "refusal", Message: "Claude declined to answer this request."}, nil, nil
	case anthropic.StopReasonPauseTurn:
		if err != nil {
			log.Printf("llm[%s]: paused turn ended with a stream error: %v", c.logName(), err)
			return Result{Code: "stream-cut", Message: "The answer was cut off. Try again."}, nil, nil
		}
		if accErr != nil {
			log.Printf("llm[%s]: paused turn could not be accumulated: %v", c.logName(), accErr)
			return Result{Code: "upstream", Message: ClaudeProvider + " could not resume the search. Try again."}, nil, nil
		}
		return Result{Code: "pause-turn"}, acc, nil
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
	default: // tool_use: nothing here asks for one
		log.Printf("llm[%s]: unexpected stop reason %q", c.logName(), stop)
		return Result{Code: "upstream", Message: ClaudeProvider + " stopped unexpectedly (" + string(stop) + "). Try again."}, nil, nil
	}
	if textChars == 0 && priorText == 0 {
		log.Printf("llm[%s]: empty answer (stop %q)", c.logName(), stop)
		return Result{Code: "empty-stream"}, nil, nil
	}
	return Result{OK: true}, acc, nil
}
