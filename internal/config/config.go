// Package config loads ai-service's runtime configuration from environment
// variables (see .env.example).
package config

import (
	"os"
	"strconv"
)

type Config struct {
	ListenAddr string // where the Go server binds (loopback; nothing fronts it — loopback is the trust boundary)
	GroqAPIKey string // /api/transcribe: comart's mic recorder (Exscriptor) -> Groq Whisper
	GroqModel  string

	// Verbatim import transcription (/api/transcribe-verbatim) runs on
	// Azure Speech (MAI-Transcribe-2) via the Fast Transcription API.
	AzureSpeechKey      string
	AzureSpeechEndpoint string // e.g. https://eastus.api.cognitive.microsoft.com

	// Word explanations (/api/explain) run on OpenRouter.
	OpenRouterAPIKey string
	ExplainModel     string // OpenRouter model id (google/gemini-3.8-flash)
	// ExplainReasoningTokens is the thinking budget, sent as OpenRouter's
	// reasoning.max_tokens (Gemini's thinkingBudget). Thinking is never shown.
	ExplainReasoningTokens int

	// Memoir (/api/memoir/*) runs on Cerebras.
	CerebrasAPIKey string
	MemoirModel    string // Cerebras model id (qwen-3.8-27b)
	// MemoirReasoningEffort is Cerebras' reasoning_effort: low, medium or
	// high; "none" turns reasoning off (disable_reasoning). Keep reasoning
	// on: without it Qwen breaks the JSON contract and edits text it must
	// leave alone.
	MemoirReasoningEffort string
}

// Load reads config from the environment. It also returns the missing API
// keys; the caller only warns, because each handler reports its own missing
// piece instead of the whole server failing.
func Load() (Config, []string) {
	c := Config{
		ListenAddr: env("LISTEN_ADDR", "127.0.0.1:8770"),
		GroqAPIKey: os.Getenv("GROQ_API_KEY"),
		GroqModel:  env("GROQ_MODEL", "whisper-large-v3-turbo"),

		AzureSpeechKey:      os.Getenv("AZURE_SPEECH_KEY"),
		AzureSpeechEndpoint: env("AZURE_SPEECH_ENDPOINT", "https://eastus.api.cognitive.microsoft.com"),

		OpenRouterAPIKey:       os.Getenv("OPENROUTER_API_KEY"),
		ExplainModel:           env("EXPLAIN_MODEL", "google/gemini-3.8-flash"),
		ExplainReasoningTokens: envInt("EXPLAIN_REASONING_TOKENS", 1024),

		CerebrasAPIKey:        os.Getenv("CEREBRAS_API_KEY"),
		MemoirModel:           env("MEMOIR_MODEL", "qwen-3.8-27b"),
		MemoirReasoningEffort: env("MEMOIR_REASONING_EFFORT", "medium"),
	}

	var missing []string
	for _, k := range []struct{ name, val string }{
		{"GROQ_API_KEY", c.GroqAPIKey},
		{"OPENROUTER_API_KEY", c.OpenRouterAPIKey},
		{"CEREBRAS_API_KEY", c.CerebrasAPIKey},
		{"AZURE_SPEECH_KEY", c.AzureSpeechKey},
	} {
		if k.val == "" {
			missing = append(missing, k.name)
		}
	}
	return c, missing
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
