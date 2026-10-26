package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OpenRouter's speech-to-text endpoint. MAI-Transcribe-2 is routed to Azure by
// OpenRouter, but the request and response are OpenRouter's JSON shape:
// base64 audio in, text plus optional verbose segments/words out. Both
// transcription routes use it, with the same OPENROUTER_TRANSCRIBE_API_KEY.
var openRouterSTTURL = "https://openrouter.ai/api/v1/audio/transcriptions"

const (
	openRouterMAIModel = "microsoft/mai-transcribe-2"

	// openRouterSTTMaxBytes is the audio ceiling for MAI on OpenRouter. The
	// docs cap Azure-backed STT uploads at 25 MB whether multipart or base64
	// JSON; base64 itself adds ~33% on the wire.
	openRouterSTTMaxBytes = 25 * 1024 * 1024

	// OpenRouter's upstream provider deadline is 60 seconds. These are local
	// backstops: the short one for a mic clip, the longer one for a verbatim
	// import that may have a slower upload.
	openRouterTranscribeTimeout = 3 * time.Minute
	openRouterVerbatimTimeout   = 3 * time.Minute

	// The endpoint answers 429 while it autoscales and 5xx for transient
	// provider failures. Retry those twice with 1 s and 2 s backoff, like the
	// old Azure path did. A Retry-After over 5 s is not worth blocking an
	// interactive recording on.
	openRouterMaxAttempts = 3
)

var openRouterRetryBase = time.Second // a variable so tests need not sleep

// openRouterSTTRequest is the JSON body of POST /api/v1/audio/transcriptions.
// []byte marshals as base64, which is exactly what input_audio.data wants.
type openRouterSTTRequest struct {
	Model                  string              `json:"model"`
	InputAudio             openRouterInput     `json:"input_audio"`
	Language               string              `json:"language,omitempty"`
	ResponseFormat         string              `json:"response_format,omitempty"`
	TimestampGranularities []string            `json:"timestamp_granularities,omitempty"`
	Provider               *openRouterProvider `json:"provider,omitempty"`
}

type openRouterInput struct {
	Data   []byte `json:"data"`
	Format string `json:"format"`
}

type openRouterProvider struct {
	Options map[string]any `json:"options"`
}

// openRouterSTTResponse covers both the plain {text, usage} answer and the
// verbose_json shape used by /api/transcribe-verbatim.
type openRouterSTTResponse struct {
	Text     string              `json:"text"`
	Duration float64             `json:"duration"`
	Language string              `json:"language"`
	Segments []openRouterSegment `json:"segments"`
	Words    []openRouterWord    `json:"words"`
	Usage    struct {
		Seconds float64 `json:"seconds"`
		Cost    float64 `json:"cost"`
	} `json:"usage"`
}

type openRouterSegment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type openRouterWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// transcriptLine and transcriptWord are the client-facing shape of
// /api/transcribe-verbatim. The names are provider-neutral; the JSON field
// names are the long-standing contract daily-dictation parses.
type transcriptLine struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
}

type transcriptWord struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// openRouterError is OpenRouter's error envelope, used only to make logs useful.
type openRouterError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// newOpenRouterSTTBody builds one request. verbose turns on segment and word
// timestamps and asks Azure for the verbatim transcript style, which is what
// daily-dictation scores against.
func newOpenRouterSTTBody(audio []byte, format string, verbose bool) ([]byte, error) {
	req := openRouterSTTRequest{
		Model:      openRouterMAIModel,
		InputAudio: openRouterInput{Data: audio, Format: format},
		Language:   "en",
	}
	if verbose {
		req.ResponseFormat = "verbose_json"
		req.TimestampGranularities = []string{"segment", "word"}
		req.Provider = &openRouterProvider{Options: map[string]any{
			"azure": map[string]any{
				"enhancedMode": map[string]any{
					"modelOptions": map[string]any{"transcribeStyle": "verbatim"},
				},
			},
		}}
	}
	return json.Marshal(req)
}

// openRouterSTTClient is the bounded HTTP client for OpenRouter transcription.
// Handlers add their own contexts, so the client timeout is only a backstop.
func (s *Server) openRouterSTTClient() *http.Client {
	if s.httpOpenRouter != nil {
		return s.httpOpenRouter
	}
	return s.shortClient()
}

// postOpenRouterSTT sends one encoded request. The caller owns the response.
func (s *Server) postOpenRouterSTT(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterSTTURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.OpenRouterTranscribeAPIKey)
	req.Header.Set("Content-Type", "application/json")
	return s.openRouterSTTClient().Do(req)
}

// openRouterRetryable reports the statuses worth repeating: rate limiting and
// transient gateway/provider failures, including OpenRouter's 504 and 524
// deadlines.
func openRouterRetryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		524, // edge-network timeout (Cloudflare)
		529: // provider overloaded
		return true
	}
	return false
}

func openRouterBackoff(attempt int) time.Duration {
	if openRouterRetryBase <= 0 {
		return 0
	}
	return time.Duration(1<<(attempt-1)) * openRouterRetryBase
}

// openRouterRetryWait is the delay before the next attempt: 1 s, then 2 s, or
// the provider's Retry-After when it gives one. A Retry-After over 5 s is not
// worth blocking an interactive recording on, so ok is false.
func openRouterRetryWait(res *http.Response, attempt int) (d time.Duration, ok bool) {
	d = openRouterBackoff(attempt)
	if ra := strings.TrimSpace(res.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
			if d > 5*time.Second {
				return 0, false
			}
		}
	}
	return d, true
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// logTail bounds an upstream error body before it reaches the log.
func logTail(b []byte) string {
	const max = 600
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// openRouterErrorDetail is the human part of an OpenRouter error body.
func openRouterErrorDetail(raw []byte) string {
	var e openRouterError
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return fmt.Sprintf("openrouter error %d: %s", e.Error.Code, e.Error.Message)
	}
	return logTail(raw)
}

// openRouterAudioFormat maps a filename to the format field OpenRouter expects.
// The value is the extension without the dot. MP4 audio is sent as m4a, the
// name OpenRouter documents.
func openRouterAudioFormat(filename string) (string, bool) {
	name := strings.ToLower(filename)
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	} else {
		name = ""
	}
	switch name {
	case "wav", "mp3", "flac", "m4a", "ogg", "webm", "aac":
		return name, true
	case "mp4":
		return "m4a", true
	}
	return "", false
}

// openRouterLines turns verbose segments into the lines shape clients use. If
// the provider returned no segments but did return text, the whole transcript
// is one line, so the response shape never loses the text.
func (r openRouterSTTResponse) lines() []transcriptLine {
	lines := make([]transcriptLine, 0, len(r.Segments))
	for _, s := range r.Segments {
		if t := strings.TrimSpace(s.Text); t != "" {
			lines = append(lines, transcriptLine{Text: t, Start: s.Start})
		}
	}
	if len(lines) == 0 {
		if t := strings.TrimSpace(r.Text); t != "" {
			lines = append(lines, transcriptLine{Text: t, Start: 0})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Start < lines[j].Start })
	return lines
}

// openRouterWords maps OpenRouter's words array to the words shape clients use.
func (r openRouterSTTResponse) wordList() []transcriptWord {
	words := make([]transcriptWord, 0, len(r.Words))
	for _, w := range r.Words {
		if t := strings.TrimSpace(w.Word); t != "" {
			words = append(words, transcriptWord{Text: t, Start: w.Start, End: w.End})
		}
	}
	sort.SliceStable(words, func(i, j int) bool { return words[i].Start < words[j].Start })
	return words
}

// drainBody reads and closes a response body, keeping at most a sane amount in
// memory. Error bodies are bounded by the endpoint anyway.
func drainBody(res *http.Response) []byte {
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return raw
}
