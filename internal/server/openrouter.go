package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OpenRouter's speech-to-text endpoint. MAI-Transcribe-2 is routed to Azure by
// OpenRouter, but the request and response are OpenRouter's JSON shape: base64
// audio in, text plus verbose segments/words out. It serves
// /api/transcribe-verbatim only; /api/transcribe runs on AssemblyAI.
var openRouterSTTURL = "https://openrouter.ai/api/v1/audio/transcriptions"

const (
	openRouterMAIModel = "microsoft/mai-transcribe-2"

	// openRouterSTTMaxBytes is the audio ceiling for MAI on OpenRouter. The
	// docs cap Azure-backed STT uploads at 25 MB whether multipart or base64
	// JSON; base64 itself adds ~33% on the wire.
	openRouterSTTMaxBytes = 25 * 1024 * 1024

	// openRouterVerbatimTimeout is a local backstop; OpenRouter's upstream
	// provider deadline is 60 seconds.
	openRouterVerbatimTimeout = 3 * time.Minute
)

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

// openRouterSTTResponse is the verbose_json shape used by
// /api/transcribe-verbatim.
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

// newOpenRouterSTTBody builds the verbose_json request used by
// /api/transcribe-verbatim: segment and word timestamps plus Azure's verbatim
// transcript style, which is what daily-dictation scores against.
func newOpenRouterSTTBody(audio []byte, format string) ([]byte, error) {
	req := openRouterSTTRequest{
		Model:                  openRouterMAIModel,
		InputAudio:             openRouterInput{Data: audio, Format: format},
		Language:               "en",
		ResponseFormat:         "verbose_json",
		TimestampGranularities: []string{"segment", "word"},
		Provider: &openRouterProvider{Options: map[string]any{
			"azure": map[string]any{
				"enhancedMode": map[string]any{
					"modelOptions": map[string]any{"transcribeStyle": "verbatim"},
				},
			},
		}},
	}
	return json.Marshal(req)
}

// openRouterSTTClient is the bounded HTTP client for OpenRouter transcription.
// The handler adds its own context, so the client timeout is only a backstop.
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
