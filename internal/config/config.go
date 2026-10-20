// Package config loads ai-service's runtime configuration from environment
// variables (see .env.example).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Providers that can serve /api/explain and /api/memoir/*. For each route
// exactly one is active, picked by which API key is set (Load refuses to run
// when two are).
const (
	ProviderOpenRouter = "openrouter"
	ProviderCerebras   = "cerebras"
	ProviderClaude     = "claude"
)

type Config struct {
	ListenAddr string // where the Go server binds (loopback; nothing fronts it — loopback is the trust boundary)
	GroqAPIKey string // /api/transcribe: comart's mic recorder (Exscriptor) -> Groq Whisper
	GroqModel  string

	// Verbatim import transcription (/api/transcribe-verbatim) runs on
	// Azure Speech (MAI-Transcribe-2) via the Fast Transcription API.
	AzureSpeechKey      string
	AzureSpeechEndpoint string // e.g. https://eastus.api.cognitive.microsoft.com

	// Word explanations (/api/explain) run on OpenRouter or on the Claude
	// Platform (api.anthropic.com), whichever key is set. Setting both is a
	// startup error: with two routes available it would be too easy to believe
	// one model answers while the other does.
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
	// or max). It sets how much a Claude model thinks, the only such control:
	// current models reject a token budget. Defaults to medium. OpenRouter
	// ignores it.
	ExplainEffort string

	// Memoir (/api/memoir/*) runs on Cerebras or on the Claude Platform,
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
)

// Load reads config from the environment. It also returns the missing API
// keys; the caller only warns, because each handler reports its own missing
// piece instead of the whole server failing.
//
// The error is for a configuration that must not run at all: both keys of one
// route set, a Claude key with a model id that is not Claude's, or the retired
// CEREBRAS_API_KEY still present. The caller exits on it.
func Load() (Config, []string, error) {
	c := Config{
		ListenAddr: env("LISTEN_ADDR", "127.0.0.1:8770"),
		GroqAPIKey: os.Getenv("GROQ_API_KEY"),
		GroqModel:  env("GROQ_MODEL", "whisper-large-v3-turbo"),

		AzureSpeechKey:      os.Getenv("AZURE_SPEECH_KEY"),
		AzureSpeechEndpoint: env("AZURE_SPEECH_ENDPOINT", "https://eastus.api.cognitive.microsoft.com"),

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
		{"GROQ_API_KEY", c.GroqAPIKey},
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

// resolveExplain picks the explain provider from the keys and settles the
// model. An empty key counts as unset, because .env.example ships empty lines.
func (c *Config) resolveExplain() error {
	switch {
	case c.OpenRouterAPIKey != "" && c.ClaudeAPIKey != "":
		return fmt.Errorf("both OPENROUTER_API_KEY and CLAUDE_API_KEY are set, so it is unclear which " +
			"provider answers /api/explain. Keep one, and set the other to empty (KEY=) in ai-service.env")
	case c.ClaudeAPIKey != "":
		c.ExplainProvider = ProviderClaude
		if c.ExplainModel == "" {
			c.ExplainModel = defaultClaudeModel
		}
		if !strings.HasPrefix(c.ExplainModel, "claude-") {
			return fmt.Errorf("CLAUDE_API_KEY is set but EXPLAIN_MODEL=%q is not a Claude model id "+
				"(they start with \"claude-\", for example %s). An OpenRouter id such as "+
				"google/gemini-3.8-flash only works with OPENROUTER_API_KEY. Delete the EXPLAIN_MODEL "+
				"line to use the default", c.ExplainModel, defaultClaudeModel)
		}
		if c.ExplainEffort == "" {
			c.ExplainEffort = defaultClaudeEffort
		}
		switch c.ExplainEffort {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("EXPLAIN_EFFORT=%q is not one of low, medium, high, xhigh, max", c.ExplainEffort)
		}
	case c.OpenRouterAPIKey != "":
		c.ExplainProvider = ProviderOpenRouter
		if c.ExplainModel == "" {
			c.ExplainModel = defaultOpenRouterModel
		}
	default:
		// No key: the handler reports it. Show the model the OpenRouter default
		// would use, so the startup line is not blank.
		if c.ExplainModel == "" {
			c.ExplainModel = defaultOpenRouterModel
		}
	}
	return nil
}

// resolveMemoir picks the Memoir provider from the keys and settles the model,
// the way resolveExplain does for /api/explain.
func (c *Config) resolveMemoir() error {
	switch {
	case c.MemoirCerebrasAPIKey != "" && c.MemoirClaudeAPIKey != "":
		return fmt.Errorf("both MEMOIR_CEREBRAS_API_KEY and MEMOIR_CLAUDE_API_KEY are set, so it is unclear which " +
			"provider answers /api/memoir/*. Keep one, and set the other to empty (KEY=) or comment it out in ai-service.env")
	case c.MemoirClaudeAPIKey != "":
		c.MemoirProvider = ProviderClaude
		if c.MemoirModel == "" {
			c.MemoirModel = defaultClaudeModel
		}
		if !strings.HasPrefix(c.MemoirModel, "claude-") {
			return fmt.Errorf("MEMOIR_CLAUDE_API_KEY is set but MEMOIR_MODEL=%q is not a Claude model id "+
				"(they start with \"claude-\", for example %s). A Cerebras id such as "+
				"qwen-3.8-27b only works with MEMOIR_CEREBRAS_API_KEY. Delete the MEMOIR_MODEL "+
				"line to use the default", c.MemoirModel, defaultClaudeModel)
		}
		if c.MemoirEffort == "" {
			c.MemoirEffort = defaultClaudeEffort
		}
		switch c.MemoirEffort {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("MEMOIR_EFFORT=%q is not one of low, medium, high, xhigh, max", c.MemoirEffort)
		}
	case c.MemoirCerebrasAPIKey != "":
		c.MemoirProvider = ProviderCerebras
		if c.MemoirModel == "" {
			c.MemoirModel = defaultCerebrasModel
		}
	default:
		// No key: the handlers report it. Show the Claude default, the one a
		// key added later selects, so the startup line is not blank.
		if c.MemoirModel == "" {
			c.MemoirModel = defaultClaudeModel
		}
	}
	return nil
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

// MemoirRoute is one line saying where /api/memoir/* goes, for the startup log.
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
