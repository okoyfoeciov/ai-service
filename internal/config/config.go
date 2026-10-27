// Package config loads ai-service's runtime configuration from environment
// variables (see .env.example).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Providers that can serve /api/transcribe/stream, /api/explain and /api/memoir/check.
// /api/transcribe/stream and /api/explain may each have every configured
// provider enabled at once; the tray picker selects the active one. Memoir
// activates exactly one, picked by which environment variable is set (Load
// refuses both).
const (
	ProviderAssemblyAI = "assemblyai"
	ProviderDeepgram   = "deepgram"

	ProviderOpenRouter = "openrouter"
	ProviderCerebras   = "cerebras"
	ProviderClaude     = "claude"
)

type Config struct {
	ListenAddr string // where the Go server binds (loopback; nothing fronts it — loopback is the trust boundary)

	// Mic transcription (/api/transcribe/stream, comart's Exscriptor) runs on
	// AssemblyAI's Universal-3.5 Pro or Deepgram Nova-3, both live APIs. At
	// least one key is required: Load refuses to start without a provider.
	AssemblyAIKey  string
	DeepgramAPIKey string
	// OpenRouterTranscribeAPIKey is OpenRouter's key for MAI-Transcribe-2,
	// which serves /api/transcribe-verbatim (daily-dictation). It is
	// deliberately not OPENROUTER_API_KEY: that one belongs to /api/explain,
	// and the two routes may use different keys.
	OpenRouterTranscribeAPIKey string
	// TranscribeProviders lists every configured provider in a fixed order. It
	// has at least one entry after Load succeeds; the tray picker appears when
	// there is more than one.
	TranscribeProviders []string
	// TranscribeProvider is the provider /api/transcribe/stream starts on: the first
	// entry of TranscribeProviders, unless main restores a selection saved by
	// the tray picker.
	TranscribeProvider string

	// Word explanations (/api/explain) run on Claude Platform or OpenRouter.
	// Both may be configured at once; the tray picker then chooses which one
	// answers. At least one key is needed for the feature, but a missing key
	// only makes the handler report it at request time.
	OpenRouterAPIKey string
	ClaudeAPIKey     string
	// ExplainProviders lists every configured explain provider in a fixed
	// order (Claude, OpenRouter). It is empty when neither key is set.
	ExplainProviders []string
	// ExplainProvider is the provider /api/explain starts on: the first entry
	// of ExplainProviders, unless main restores a selection saved by the tray
	// picker.
	ExplainProvider string
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

// Defaults for the fixed explain models and the Memoir model/effort when
// MEMOIR_MODEL (and the effort) are unset.
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
// The error is for a configuration that must not run at all: more than one
// variable of the Memoir route set, neither ASSEMBLYAI_API_KEY nor
// DEEPGRAM_API_KEY set, a retired transcription variable still present
// (PARAKEET_URL, MAI_TRANSCRIBE_API_KEY, AZURE_SPEECH_KEY,
// AZURE_SPEECH_ENDPOINT, GROQ_API_KEY, GROQ_MODEL), a Claude key with a model id
// that is not Claude's or an effort not in low/medium/high/xhigh/max, or the
// retired CEREBRAS_API_KEY still present. The caller exits on it.
func Load() (Config, []string, error) {
	c := Config{
		ListenAddr:                 env("LISTEN_ADDR", "127.0.0.1:8770"),
		AssemblyAIKey:              os.Getenv("ASSEMBLYAI_API_KEY"),
		DeepgramAPIKey:             os.Getenv("DEEPGRAM_API_KEY"),
		OpenRouterTranscribeAPIKey: os.Getenv("OPENROUTER_TRANSCRIBE_API_KEY"),

		OpenRouterAPIKey:       os.Getenv("OPENROUTER_API_KEY"),
		ClaudeAPIKey:           os.Getenv("CLAUDE_API_KEY"),
		ExplainReasoningTokens: envInt("EXPLAIN_REASONING_TOKENS", 1024),
		ExplainEffort:          strings.ToLower(os.Getenv("EXPLAIN_EFFORT")),

		MemoirCerebrasAPIKey:  os.Getenv("MEMOIR_CEREBRAS_API_KEY"),
		MemoirClaudeAPIKey:    os.Getenv("MEMOIR_CLAUDE_API_KEY"),
		MemoirModel:           os.Getenv("MEMOIR_MODEL"),
		MemoirReasoningEffort: env("MEMOIR_REASONING_EFFORT", "medium"),
		MemoirEffort:          strings.ToLower(os.Getenv("MEMOIR_EFFORT")),
	}

	// Parakeet was removed. Ignoring a leftover URL silently would leave a
	// setting in the file that does nothing while /api/transcribe/stream uses
	// AssemblyAI or Deepgram.
	if os.Getenv("PARAKEET_URL") != "" {
		return c, nil, fmt.Errorf("PARAKEET_URL is no longer supported (the local Parakeet integration was removed). " +
			"Delete the line from ai-service.env; /api/transcribe/stream uses ASSEMBLYAI_API_KEY or DEEPGRAM_API_KEY")
	}
	// Groq was removed when /api/transcribe/stream moved to AssemblyAI and
	// Deepgram. A leftover key or model would sit in the file doing nothing.
	var retiredGroq []string
	for _, k := range []string{"GROQ_API_KEY", "GROQ_MODEL"} {
		if os.Getenv(k) != "" {
			retiredGroq = append(retiredGroq, k)
		}
	}
	if len(retiredGroq) > 0 {
		verb := "is"
		if len(retiredGroq) > 1 {
			verb = "are"
		}
		return c, nil, fmt.Errorf("%s %s no longer used: /api/transcribe/stream now runs on "+
			"AssemblyAI or Deepgram. Delete the retired line(s) from ai-service.env",
			strings.Join(retiredGroq, ", "), verb)
	}
	// The Azure Speech path was replaced by OpenRouter. The old variables must
	// not stay behind doing nothing: verbatim transcription now uses the
	// OpenRouter key.
	var retired []string
	for _, k := range []string{"MAI_TRANSCRIBE_API_KEY", "AZURE_SPEECH_KEY", "AZURE_SPEECH_ENDPOINT"} {
		if os.Getenv(k) != "" {
			retired = append(retired, k)
		}
	}
	if len(retired) > 0 {
		verb := "is"
		if len(retired) > 1 {
			verb = "are"
		}
		return c, nil, fmt.Errorf("%s %s no longer used: MAI-Transcribe-2 now runs through "+
			"OpenRouter, and /api/transcribe-verbatim reads OPENROUTER_TRANSCRIBE_API_KEY. "+
			"Delete the retired line(s) from ai-service.env",
			strings.Join(retired, ", "), verb)
	}
	if err := c.resolveTranscribe(); err != nil {
		return c, nil, err
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
		{"OPENROUTER_API_KEY or CLAUDE_API_KEY", c.ExplainProvider},
		{"MEMOIR_CEREBRAS_API_KEY or MEMOIR_CLAUDE_API_KEY", c.MemoirProvider},
		{"OPENROUTER_TRANSCRIBE_API_KEY", c.OpenRouterTranscribeAPIKey},
	} {
		if k.val == "" {
			missing = append(missing, k.name)
		}
	}
	return c, missing, nil
}

// resolveTranscribe collects the /api/transcribe/stream providers. An empty
// variable counts as unset, because .env.example ships empty lines.
func (c *Config) resolveTranscribe() error {
	if c.AssemblyAIKey != "" {
		c.TranscribeProviders = append(c.TranscribeProviders, ProviderAssemblyAI)
	}
	if c.DeepgramAPIKey != "" {
		c.TranscribeProviders = append(c.TranscribeProviders, ProviderDeepgram)
	}
	if len(c.TranscribeProviders) == 0 {
		return fmt.Errorf("neither ASSEMBLYAI_API_KEY nor DEEPGRAM_API_KEY is set, so " +
			"/api/transcribe/stream has no provider. Set at least one in ai-service.env")
	}
	c.TranscribeProvider = c.TranscribeProviders[0]
	return nil
}

// resolveExplain collects the /api/explain providers. Both keys may be set; the
// tray picker then switches between them. An empty key counts as unset, because
// .env.example ships empty lines. Models are fixed per provider.
func (c *Config) resolveExplain() error {
	if c.ClaudeAPIKey != "" {
		c.ExplainProviders = append(c.ExplainProviders, ProviderClaude)
	}
	if c.OpenRouterAPIKey != "" {
		c.ExplainProviders = append(c.ExplainProviders, ProviderOpenRouter)
	}
	if len(c.ExplainProviders) == 0 {
		return nil
	}
	c.ExplainProvider = c.ExplainProviders[0]
	if c.ClaudeAPIKey != "" {
		if err := settleEffort(&c.ExplainEffort, "EXPLAIN_EFFORT"); err != nil {
			return err
		}
	}
	return nil
}

// ExplainModelFor returns the fixed model id for provider.
func (c Config) ExplainModelFor(provider string) string {
	switch provider {
	case ProviderClaude:
		return defaultClaudeModel
	case ProviderOpenRouter:
		return defaultOpenRouterModel
	}
	return ""
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
	return settleEffort(effort, effortVar)
}

// settleEffort defaults and validates a Claude effort level.
func settleEffort(effort *string, effortVar string) error {
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

// TranscribeRoute is one line saying where /api/transcribe/stream goes, for the startup log.
func (c Config) TranscribeRoute() string {
	var parts []string
	for _, p := range c.TranscribeProviders {
		switch p {
		case ProviderAssemblyAI:
			parts = append(parts, "AssemblyAI live, model universal-3-5-pro")
		case ProviderDeepgram:
			parts = append(parts, "Deepgram live, model nova-3")
		}
	}
	switch len(parts) {
	case 0:
		return "none (neither ASSEMBLYAI_API_KEY nor DEEPGRAM_API_KEY is set)"
	case 1:
		return parts[0]
	}
	return strings.Join(parts, " and ") +
		fmt.Sprintf(" (%d providers; tray picker, starting on %s)", len(parts), transcribeProviderName(c.TranscribeProvider))
}

// transcribeProviderName is the short human name used in the startup log.
func transcribeProviderName(provider string) string {
	switch provider {
	case ProviderAssemblyAI:
		return "AssemblyAI Universal-3.5 Pro"
	case ProviderDeepgram:
		return "Deepgram Nova-3"
	}
	return provider
}

// ExplainRoute is one line saying where /api/explain goes, for the startup log.
func (c Config) ExplainRoute() string {
	var parts []string
	for _, p := range c.ExplainProviders {
		switch p {
		case ProviderClaude:
			parts = append(parts, fmt.Sprintf("Claude Platform, model %s, effort %s", c.ExplainModelFor(p), c.ExplainEffort))
		case ProviderOpenRouter:
			parts = append(parts, fmt.Sprintf("OpenRouter, model %s, %d thinking tokens", c.ExplainModelFor(p), c.ExplainReasoningTokens))
		}
	}
	switch len(parts) {
	case 0:
		return "none (neither OPENROUTER_API_KEY nor CLAUDE_API_KEY is set)"
	case 1:
		return parts[0]
	}
	return strings.Join(parts, " and ") +
		fmt.Sprintf(" (%d providers; tray picker, starting on %s)", len(parts), explainProviderName(c.ExplainProvider))
}

// explainProviderName is the short human name used in the startup log.
func explainProviderName(provider string) string {
	switch provider {
	case ProviderClaude:
		return "Claude Platform"
	case ProviderOpenRouter:
		return "OpenRouter"
	}
	return provider
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
