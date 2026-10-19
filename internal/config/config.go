// Package config loads ai-service's runtime configuration from environment
// variables (see .env.example).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Providers that can serve /api/explain. Exactly one is active, picked by
// which API key is set (Load refuses to run when both are).
const (
	ProviderOpenRouter = "openrouter"
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

	// Memoir (/api/memoir/*) runs on Cerebras.
	CerebrasAPIKey string
	MemoirModel    string // Cerebras model id (qwen-3.8-27b)
	// MemoirReasoningEffort is Cerebras' reasoning_effort: low, medium or
	// high; "none" turns reasoning off (disable_reasoning). Keep reasoning
	// on: without it Qwen breaks the JSON contract and edits text it must
	// leave alone.
	MemoirReasoningEffort string
}

// Defaults for /api/explain when EXPLAIN_MODEL (and EXPLAIN_EFFORT) is unset.
const (
	defaultOpenRouterModel = "google/gemini-3.8-flash"
	defaultClaudeModel     = "claude-opus-5-5"
	defaultClaudeEffort    = "medium"
)

// Load reads config from the environment. It also returns the missing API
// keys; the caller only warns, because each handler reports its own missing
// piece instead of the whole server failing.
//
// The error is for a configuration that must not run at all: both explain keys
// set, or a Claude key with a model id that is not Claude's. The caller exits
// on it.
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

		CerebrasAPIKey:        os.Getenv("CEREBRAS_API_KEY"),
		MemoirModel:           env("MEMOIR_MODEL", "qwen-3.8-27b"),
		MemoirReasoningEffort: env("MEMOIR_REASONING_EFFORT", "medium"),
	}

	if err := c.resolveExplain(); err != nil {
		return c, nil, err
	}

	var missing []string
	for _, k := range []struct{ name, val string }{
		{"GROQ_API_KEY", c.GroqAPIKey},
		{"OPENROUTER_API_KEY or CLAUDE_API_KEY", c.ExplainProvider},
		{"CEREBRAS_API_KEY", c.CerebrasAPIKey},
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
