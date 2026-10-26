// Package config loads ai-service's runtime configuration from environment
// variables (see .env.example).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Providers that can serve /api/transcribe, /api/explain and /api/memoir/check.
// For each route exactly one is active, picked by which environment variable is
// set (Load refuses to run when more than one is).
const (
	ProviderGroq          = "groq"
	ProviderMAITranscribe = "mai-transcribe-2"

	ProviderOpenRouter = "openrouter"
	ProviderCerebras   = "cerebras"
	ProviderClaude     = "claude"
)

type Config struct {
	ListenAddr string // where the Go server binds (loopback; nothing fronts it — loopback is the trust boundary)

	// Mic transcription (/api/transcribe, comart's Exscriptor) runs on Groq
	// Whisper or on Azure Speech (MAI-Transcribe-2), whichever one of
	// GROQ_API_KEY or MAI_TRANSCRIBE_API_KEY is set. Setting more than one is a
	// startup error: with two providers for one route it would be too easy to
	// believe one service answers while the other does.
	GroqAPIKey          string
	GroqModel           string
	MAITranscribeAPIKey string
	// TranscribeProvider is ProviderGroq or ProviderMAITranscribe, or "" when
	// neither key is set (the transcribe handler then reports the missing
	// configuration).
	TranscribeProvider string

	// Verbatim import transcription (/api/transcribe-verbatim) runs on
	// Azure Speech (MAI-Transcribe-2) via the Fast Transcription API.
	// Separate from /api/transcribe: AZURE_SPEECH_KEY is only for
	// /api/transcribe-verbatim, while AZURE_SPEECH_ENDPOINT serves any Azure
	// Speech call.
	AzureSpeechKey      string
	AzureSpeechEndpoint string // e.g. https://eastus.api.cognitive.microsoft.com

	// Word explanations (/api/explain) run on OpenRouter or on the Claude
	// Platform (api.anthropic.com), whichever key is set. Setting both is a
	// startup error: with two providers for one route it would be too easy to
	// believe one model answers while the other does.
	OpenRouterAPIKey string
	ClaudeAPIKey     string
	// ExplainProvider is ProviderOpenRouter or ProviderClaude, or "" when
	// neither key is set (the explain handler then reports the missing key).
	ExplainProvider string
	// ExplainModel is the model id sent to the active provider: an OpenRouter id
	// (google/gemini-3.8-flash) or a Claude id (claude-opus-5-5). When
	// EXPLAIN_MODEL is unset it defaults per provider.
	ExplainModel string
	// ExplainReasoningTokens is the thinking budget, sent as OpenRouter's
	// reasoning.max_tokens (Gemini's thinkingBudget). Thinking is never shown.
	// Claude ignores it: its depth is ExplainEffort.
	ExplainReasoningTokens int
	// ExplainEffort is Claude's output_config.effort (low, medium, high, xhigh
	// or max). It is the only thinking-depth control: current models reject a
	// separate thinking-token budget, while max_tokens still caps the answer.
	// Defaults to medium. OpenRouter ignores it.
	ExplainEffort string

	// Memoir (/api/memoir/check) runs on Cerebras or on the Claude Platform,
	// whichever key is set, by the same rule as /api/explain: both set is a
	// startup error. Every Memoir variable starts with MEMOIR_, because the
	// plain CLAUDE_API_KEY already belongs to /api/explain.
	MemoirCerebrasAPIKey string
	MemoirClaudeAPIKey   string
	// MemoirProvider is ProviderCerebras or ProviderClaude, or "" when neither
	// key is set (the Memoir handlers then report the missing key).
	MemoirProvider string
	// MemoirModel is the model id sent to the active provider: a Cerebras id
	// (qwen-3.8-27b) or a Claude id (claude-opus-5-5). When MEMOIR_MODEL is
	// unset it defaults per provider.
	MemoirModel string
	// MemoirReasoningEffort is Cerebras' reasoning_effort: low, medium or
	// high; "none" turns reasoning off (disable_reasoning). Keep reasoning
	// on: without it Qwen breaks the JSON contract and edits text it must
	// leave alone. Claude ignores it: its depth is MemoirEffort.
	MemoirReasoningEffort string
	// MemoirEffort is Claude's output_config.effort (low, medium, high, xhigh
	// or max), the only thinking control a Claude model takes. Defaults to
	// medium. Cerebras ignores it.
	MemoirEffort string
}

// Defaults for the model and effort when EXPLAIN_MODEL / MEMOIR_MODEL (and the
// effort) are unset.
const (
	defaultOpenRouterModel = "google/gemini-3.8-flash"
	defaultCerebrasModel   = "qwen-3.8-27b"
	defaultClaudeModel     = "claude-opus-5-5"
	defaultClaudeEffort    = "medium"

	// DefaultAzureSpeechEndpoint is the Azure region both transcription routes
	// use when AZURE_SPEECH_ENDPOINT is unset.
	DefaultAzureSpeechEndpoint = "https://eastus.api.cognitive.microsoft.com"
)

// Load reads config from the environment. It also returns the missing API
// keys; the caller only warns, because each handler reports its own missing
// piece instead of the whole server failing.
//
// The error is for a configuration that must not run at all: more than one
// variable of one route set, the retired PARAKEET_URL still present, a Claude
// key with a model id that is not Claude's or an effort not in
// low/medium/high/xhigh/max, or the retired CEREBRAS_API_KEY still present. The
// caller exits on it.
func Load() (Config, []string, error) {
	c := Config{
		ListenAddr:          env("LISTEN_ADDR", "127.0.0.1:8770"),
		GroqAPIKey:          os.Getenv("GROQ_API_KEY"),
		GroqModel:           env("GROQ_MODEL", "whisper-large-v3-turbo"),
		MAITranscribeAPIKey: os.Getenv("MAI_TRANSCRIBE_API_KEY"),

		AzureSpeechKey:      os.Getenv("AZURE_SPEECH_KEY"),
		AzureSpeechEndpoint: env("AZURE_SPEECH_ENDPOINT", DefaultAzureSpeechEndpoint),

		OpenRouterAPIKey:       os.Getenv("OPENROUTER_API_KEY"),
		ClaudeAPIKey:           os.Getenv("CLAUDE_API_KEY"),
		ExplainModel:           os.Getenv("EXPLAIN_MODEL"),
		ExplainReasoningTokens: envInt("EXPLAIN_REASONING_TOKENS", 1024),
		ExplainEffort:          strings.ToLower(os.Getenv("EXPLAIN_EFFORT")),

		MemoirCerebrasAPIKey:  os.Getenv("MEMOIR_CEREBRAS_API_KEY"),
		MemoirClaudeAPIKey:    os.Getenv("MEMOIR_CLAUDE_API_KEY"),
		MemoirModel:           os.Getenv("MEMOIR_MODEL"),
		MemoirReasoningEffort: env("MEMOIR_REASONING_EFFORT", "medium"),
		MemoirEffort:          strings.ToLower(os.Getenv("MEMOIR_EFFORT")),
	}

	if err := c.resolveTranscribe(); err != nil {
		return c, nil, err
	}
	// Parakeet was removed. Ignoring a leftover URL silently would leave a
	// setting in the file that does nothing while /api/transcribe uses Groq or
	// Azure.
	if os.Getenv("PARAKEET_URL") != "" {
		return c, nil, fmt.Errorf("PARAKEET_URL is no longer supported (the local Parakeet integration was removed). " +
			"Delete the line from ai-service.env; /api/transcribe uses GROQ_API_KEY or MAI_TRANSCRIBE_API_KEY")
	}
	if err := c.resolveExplain(); err != nil {
		return c, nil, err
	}
	// The old name is not read any more. Ignoring it silently would leave a
	// key in the file that does nothing while Memoir reports it is missing.
	if os.Getenv("CEREBRAS_API_KEY") != "" {
		return c, nil, fmt.Errorf("CEREBRAS_API_KEY was renamed MEMOIR_CEREBRAS_API_KEY (every Memoir " +
			"variable starts with MEMOIR_). Rename it, or comment the line out to stop using Cerebras, in ai-service.env")
	}
	if err := c.resolveMemoir(); err != nil {
		return c, nil, err
	}

	var missing []string
	for _, k := range []struct{ name, val string }{
		{"GROQ_API_KEY or MAI_TRANSCRIBE_API_KEY", c.TranscribeProvider},
		{"OPENROUTER_API_KEY or CLAUDE_API_KEY", c.ExplainProvider},
		{"MEMOIR_CEREBRAS_API_KEY or MEMOIR_CLAUDE_API_KEY", c.MemoirProvider},
		{"AZURE_SPEECH_KEY", c.AzureSpeechKey},
	} {
		if k.val == "" {
			missing = append(missing, k.name)
		}
	}
	return c, missing, nil
}

// resolveTranscribe picks the /api/transcribe provider from the two
// environment variables. An empty variable counts as unset, because
// .env.example ships empty lines.
func (c *Config) resolveTranscribe() error {
	var set []string
	if c.GroqAPIKey != "" {
		set = append(set, "GROQ_API_KEY")
	}
	if c.MAITranscribeAPIKey != "" {
		set = append(set, "MAI_TRANSCRIBE_API_KEY")
	}
	if len(set) > 1 {
		return fmt.Errorf("%s are set, so it is unclear which "+
			"provider answers /api/transcribe. Keep one, and set the other to empty (KEY=) or comment it out in ai-service.env",
			strings.Join(set, " and "))
	}
	switch {
	case c.GroqAPIKey != "":
		c.TranscribeProvider = ProviderGroq
	case c.MAITranscribeAPIKey != "":
		c.TranscribeProvider = ProviderMAITranscribe
	}
	return nil
}

// resolveExplain picks the explain provider from the keys and settles the
// model. An empty key counts as unset, because .env.example ships empty lines.
func (c *Config) resolveExplain() error {
	switch {
	case c.OpenRouterAPIKey != "" && c.ClaudeAPIKey != "":
		return fmt.Errorf("both OPENROUTER_API_KEY and CLAUDE_API_KEY are set, so it is unclear which " +
			"provider answers /api/explain. Keep one, and set the other to empty (KEY=) in ai-service.env")
	case c.ClaudeAPIKey != "":
		c.ExplainProvider = ProviderClaude
		return settleClaude(&c.ExplainModel, &c.ExplainEffort, "CLAUDE_API_KEY",
			"EXPLAIN_MODEL", "EXPLAIN_EFFORT",
			"An OpenRouter id such as google/gemini-3.8-flash only works with OPENROUTER_API_KEY")
	case c.OpenRouterAPIKey != "":
		c.ExplainProvider = ProviderOpenRouter
		if c.ExplainModel == "" {
			c.ExplainModel = defaultOpenRouterModel
		}
	}
	return nil
}

// settleClaude applies the defaults and checks shared by the two Claude routes.
// keyVar, modelVar and effortVar name the environment variables in errors;
// otherProvider explains which alternate key the model id belongs to.
func settleClaude(model, effort *string, keyVar, modelVar, effortVar, otherProvider string) error {
	if *model == "" {
		*model = defaultClaudeModel
	}
	if !strings.HasPrefix(*model, "claude-") {
		return fmt.Errorf("%s is set but %s=%q is not a Claude model id "+
			"(they start with \"claude-\", for example %s). %s. Delete the %s "+
			"line to use the default", keyVar, modelVar, *model, defaultClaudeModel, otherProvider, modelVar)
	}
	if *effort == "" {
		*effort = defaultClaudeEffort
	}
	switch *effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("%s=%q is not one of low, medium, high, xhigh, max", effortVar, *effort)
	}
	return nil
}

// resolveMemoir picks the Memoir provider from the keys and settles the model,
// the way resolveExplain does for /api/explain.
func (c *Config) resolveMemoir() error {
	switch {
	case c.MemoirCerebrasAPIKey != "" && c.MemoirClaudeAPIKey != "":
		return fmt.Errorf("both MEMOIR_CEREBRAS_API_KEY and MEMOIR_CLAUDE_API_KEY are set, so it is unclear which " +
			"provider answers /api/memoir/check. Keep one, and set the other to empty (KEY=) or comment it out in ai-service.env")
	case c.MemoirClaudeAPIKey != "":
		c.MemoirProvider = ProviderClaude
		return settleClaude(&c.MemoirModel, &c.MemoirEffort, "MEMOIR_CLAUDE_API_KEY",
			"MEMOIR_MODEL", "MEMOIR_EFFORT",
			"A Cerebras id such as qwen-3.8-27b only works with MEMOIR_CEREBRAS_API_KEY")
	case c.MemoirCerebrasAPIKey != "":
		c.MemoirProvider = ProviderCerebras
		if c.MemoirModel == "" {
			c.MemoirModel = defaultCerebrasModel
		}
	}
	return nil
}

// TranscribeRoute is one line saying where /api/transcribe goes, for the startup log.
func (c Config) TranscribeRoute() string {
	switch c.TranscribeProvider {
	case ProviderGroq:
		return fmt.Sprintf("Groq, model %s", c.GroqModel)
	case ProviderMAITranscribe:
		return fmt.Sprintf("Azure Speech, model MAI-Transcribe-2 (%s)", c.AzureSpeechEndpoint)
	}
	return "none (neither GROQ_API_KEY nor MAI_TRANSCRIBE_API_KEY is set)"
}

// ExplainRoute is one line saying where /api/explain goes, for the startup log.
func (c Config) ExplainRoute() string {
	switch c.ExplainProvider {
	case ProviderClaude:
		return fmt.Sprintf("Claude Platform, model %s, effort %s", c.ExplainModel, c.ExplainEffort)
	case ProviderOpenRouter:
		return fmt.Sprintf("OpenRouter, model %s, %d thinking tokens", c.ExplainModel, c.ExplainReasoningTokens)
	}
	return "none (neither OPENROUTER_API_KEY nor CLAUDE_API_KEY is set)"
}

// MemoirRoute is one line saying where /api/memoir/check goes, for the startup log.
func (c Config) MemoirRoute() string {
	switch c.MemoirProvider {
	case ProviderClaude:
		return fmt.Sprintf("Claude Platform, model %s, effort %s", c.MemoirModel, c.MemoirEffort)
	case ProviderCerebras:
		return fmt.Sprintf("Cerebras, model %s, reasoning %s", c.MemoirModel, c.MemoirReasoningEffort)
	}
	return "none (neither MEMOIR_CEREBRAS_API_KEY nor MEMOIR_CLAUDE_API_KEY is set)"
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
