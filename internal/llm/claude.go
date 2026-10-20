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
	// Route says which route this client serves ("explain", "memoir"), for the
	// log only: two Claude clients with the same model would otherwise log
	// identical lines.
	Route string
}

// Claude streams an answer from the Claude Platform's Messages API with the
// official SDK. It has the same Stream contract as Client, so a handler can
// use either.
//
// The request carries the model, max_tokens, adaptive thinking and the effort,
// and nothing else: no tools, no sampling settings, and no refusal fallback. A
// fallback would answer from another model after a refusal, which is the silent
// re-routing this service is set up to avoid; a refusal is reported as an error
// instead. Adaptive thinking is asked for explicitly because leaving the field
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
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.cfg.Model),
		MaxTokens: c.cfg.MaxTokens,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
	}
	if c.cfg.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(c.cfg.Effort)}
	}

	for attempt := 0; ; attempt++ {
		stream := c.client.Messages.NewStreaming(ctx, params)
		res, httpErr := c.read(ctx, stream, onText)
		_ = stream.Close()
		if httpErr == nil {
			return res
		}

		raw := []byte(httpErr.RawJSON())
		wait, retry := retryPlan(httpErr.StatusCode, httpErr.Response.Header, attempt)
		if retry {
			log.Printf("llm[%s]: HTTP %d, retrying: %s", c.logName(), httpErr.StatusCode, truncate(string(raw), 200))
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

// read consumes the stream. It returns a non-nil *anthropic.Error, and no
// Result, when the API answered the request itself with an HTTP error: Stream
// decides whether to retry that. Everything else, including an error event
// inside an open stream, is a Result. As in Client.read, the answer is judged at
// the end, so no caller takes a partial answer for a whole one.
func (c *Claude) read(ctx context.Context, stream interface {
	Next() bool
	Current() anthropic.MessageStreamEventUnion
	Err() error
}, onText func(string)) (Result, *anthropic.Error) {
	var (
		textChars int
		stop      anthropic.StopReason
		details   anthropic.RefusalStopDetails
	)
	for stream.Next() {
		switch ev := stream.Current().AsAny().(type) {
		case anthropic.MessageStartEvent:
			// Say which model really answered, next to the one asked for: the
			// two lines in the log are the check that a request went where the
			// config says.
			log.Printf("llm[%s]: asked %s, answered by %s (message %s)", c.logName(), c.cfg.Model, ev.Message.Model, ev.Message.ID)
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
		return Result{Code: "cancelled"}, nil // the caller went away; not a provider failure
	}
	var apiErr *anthropic.Error
	isAPI := errors.As(err, &apiErr)
	if isAPI && apiErr.Response != nil && apiErr.StatusCode >= 400 {
		return Result{}, apiErr // the request itself was refused
	}
	if err != nil && !isAPI && stop == "" && textChars == 0 {
		// Nothing arrived: the host was not reached, or the connection broke
		// before the first event.
		log.Printf("llm[%s]: request failed: %v", c.logName(), err)
		return Result{Code: "network", Message: ClaudeProvider + " is unreachable (" + err.Error() + ")."}, nil
	}
	// The answer is whole once a stop reason has arrived, even if the connection
	// then breaks before message_stop.
	if stop == "" {
		if isAPI {
			msg := errMessage([]byte(apiErr.RawJSON()))
			log.Printf("llm[%s]: error in stream: %s", c.logName(), msg)
			return Result{Code: "upstream", Message: ClaudeProvider + " error: " + msg}, nil
		}
		log.Printf("llm[%s]: stream cut after %d chars: %v", c.logName(), textChars, err)
		return Result{Code: "stream-cut", Message: "The answer was cut off. Try again."}, nil
	}
	switch stop {
	case anthropic.StopReasonMaxTokens:
		log.Printf("llm[%s]: stopped at the token limit after %d chars", c.logName(), textChars)
		return Result{Code: "truncated", Message: "The answer hit the length limit and was cut off. Try again."}, nil
	case anthropic.StopReasonModelContextWindowExceeded:
		log.Printf("llm[%s]: stopped at the context window limit after %d chars", c.logName(), textChars)
		return Result{Code: "truncated", Message: "The request was too long for the model."}, nil
	case anthropic.StopReasonRefusal:
		log.Printf("llm[%s]: declined (category %q): %s", c.logName(), details.Category, details.Explanation)
		return Result{Code: "refusal", Message: "Claude declined to answer this request."}, nil
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
	default: // tool_use, pause_turn: nothing here asks for either
		log.Printf("llm[%s]: unexpected stop reason %q", c.logName(), stop)
		return Result{Code: "upstream", Message: ClaudeProvider + " stopped unexpectedly (" + string(stop) + "). Try again."}, nil
	}
	if textChars == 0 {
		log.Printf("llm[%s]: empty answer (stop %q)", c.logName(), stop)
		return Result{Code: "empty-stream"}, nil
	}
	return Result{OK: true}, nil
}
