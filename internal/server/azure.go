package server

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// azureAPIVersion is the Fast Transcription API version both Azure routes use.
	azureAPIVersion = "2025-10-15"
	// azureMaxBytes is the Fast Transcription per-file ceiling (300 MB / ~2 h).
	azureMaxBytes       = 300 * 1024 * 1024
	azureTranscribePath = "/speechtotext/transcriptions:transcribe?api-version=" + azureAPIVersion

	maiDefinition = `{"locales":["en"],"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2"},"diarization":{"enabled":false}}`
	// Verbatim keeps word timestamps and no diarization: it shortens the
	// supported length, and dictation scores single-track references.
	verbatimDefinition = `{"locales":["en"],"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"word","transcribeStyle":"verbatim"}},"diarization":{"enabled":false}}`

	// The fast transcription endpoint is elastic: it answers 429 while it
	// autoscales and 5xx for transient failures. Retry those twice with 1 s and
	// 2 s backoff; a Retry-After over 5 s is not worth blocking an interactive
	// recording on.
	azureMaxAttempts       = 3
	azureTranscribeTimeout = 3 * time.Minute
)

var azureRetryBase = time.Second // a variable so tests need not sleep

// azureRetryable reports the statuses Microsoft's fast transcription guidance
// says to retry: 429 while the service autoscales, and transient 5xx.
func azureRetryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func azureBackoff(attempt int) time.Duration {
	if azureRetryBase <= 0 {
		return 0
	}
	return time.Duration(1<<(attempt-1)) * azureRetryBase
}

// azureRetryWait is the delay before the next attempt: 1 s, then 2 s, or the
// provider's Retry-After when it gives one. A Retry-After over 5 s is not worth
// blocking an interactive recording on, so ok is false and the request fails.
func azureRetryWait(res *http.Response, attempt int) (d time.Duration, ok bool) {
	d = azureBackoff(attempt)
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

// azureLine and azureWord are daily-dictation's transcript shape (seconds):
// lines carry the segment text, words carry per-word timings for the dictation
// loop's phrase looping.
type azureLine struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
}

type azureWord struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type azurePhraseWord struct {
	Text       string  `json:"text"`
	OffsetMs   float64 `json:"offsetMilliseconds"`
	DurationMs float64 `json:"durationMilliseconds"`
}

type azurePhrase struct {
	Text     string            `json:"text"`
	OffsetMs float64           `json:"offsetMilliseconds"`
	Words    []azurePhraseWord `json:"words"`
}

type azureCombinedPhrase struct {
	Text string `json:"text"`
}

type azureResponse struct {
	CombinedPhrases []azureCombinedPhrase `json:"combinedPhrases"`
	Phrases         []azurePhrase         `json:"phrases"`
}

// sortedPhrases returns the phrases in offset order, leaving the response as is.
func (r azureResponse) sortedPhrases() []azurePhrase {
	phrases := append([]azurePhrase(nil), r.Phrases...)
	sort.Slice(phrases, func(i, j int) bool { return phrases[i].OffsetMs < phrases[j].OffsetMs })
	return phrases
}

// text joins the transcript in time order, falling back to combinedPhrases when
// Azure returns no per-phrase entries.
func (r azureResponse) text() string {
	var parts []string
	for _, p := range r.sortedPhrases() {
		if t := strings.TrimSpace(p.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		for _, cp := range r.CombinedPhrases {
			if t := strings.TrimSpace(cp.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, " ")
}

// azureUpload is one encoded Fast Transcription request.
type azureUpload struct {
	url         string
	body        []byte
	contentType string
}

func newAzureUpload(endpoint, fileField, filename, fileType string, audio []byte, definition string) (*azureUpload, error) {
	body, contentType, err := multipartBody(fileField, filename, fileType, audio, [2]string{"definition", definition})
	if err != nil {
		return nil, err
	}
	return &azureUpload{
		url:         strings.TrimSuffix(endpoint, "/") + azureTranscribePath,
		body:        body.Bytes(),
		contentType: contentType,
	}, nil
}

func (u *azureUpload) request(ctx context.Context, key string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(u.body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", key)
	req.Header.Set("Content-Type", u.contentType)
	return req, nil
}
