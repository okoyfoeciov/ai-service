// Package config loads ai-service's runtime configuration from environment
// variables (see .env.example).
package config

import (
	"fmt"
	"os"
	"strings"
)

// Providers that can serve /api/transcribe/stream, /api/explain, /api/ask and
// /api/memoir/check. /api/transcribe/stream, /api/explain and /api/ask may each
// have every configured provider enabled at once; the tray picker selects the
// active one. Memoir activates exactly one, picked by which environment
// variable is set (Load refuses both).
const (
	ProviderAssemblyAI = "assemblyai"
	ProviderDeepgram   = "deepgram"

	ProviderOpenRouter = "openrouter"
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
	// order (Claude, OpenRouter). It is empty when no key is set.
	ExplainProviders []string
	// ExplainProvider is the provider /api/explain starts on: the first entry
	// of ExplainProviders, unless main restores a selection saved by the tray
	// picker.
	ExplainProvider string

	// Ask (/api/ask) runs on Claude Platform or OpenRouter. Both may be
	// configured at once; the tray picker then chooses which one answers.
	// The Claude and OpenRouter keys are deliberately separate from the explain
	// and Memoir keys, so each route can keep its own credential.
	AskClaudeAPIKey     string
	AskOpenRouterAPIKey string
	// BraveSearchAPIKey enables the native image-search tools on every Ask
	// choice. It is optional: without it, /api/ask answers exactly as before
	// and simply offers no image tools.
	BraveSearchAPIKey string
	// AskProviders lists every configured ask provider in a fixed order
	// (Claude, OpenRouter). It is empty when no key is set.
	AskProviders []string
	// AskProvider is the provider /api/ask starts on: the first entry of
	// AskProviders, unless main restores a selection saved by the tray picker.
	AskProvider string

	// Memoir (/api/memoir/check) runs on the Claude Platform. Every Memoir
	// variable starts with MEMOIR_, because the plain CLAUDE_API_KEY already
	// belongs to /api/explain.
	MemoirClaudeAPIKey string
	// MemoirProvider is ProviderClaude, or "" when the key is not set (the
	// Memoir handlers then report the missing key).
	MemoirProvider string
	// MemoirModel is the Claude model id sent to /api/memoir/check
	// (claude-opus-5-5). When MEMOIR_MODEL is unset it defaults to that.
	MemoirModel string
}

// Defaults for the fixed models, and the fixed thinking controls shared by the
// routes. Models and thinking depth are deliberately not environment settings:
// every start and every tray switch must answer with the same model and the
// same reasoning depth.
const (
	defaultOpenRouterModel    = "google/gemini-3.8-flash"
	defaultOpenRouterAskModel = "deepseek/deepseek-v4.1-flash-1017"
	defaultClaudeModel        = "claude-opus-5-5"

	// ClaudeEffort is Claude's output_config.effort (low, medium, high, xhigh
	// or max), the only thinking control a Claude model takes. Current models
	// reject a separate thinking-token budget, while max_tokens still caps the
	// answer.
	ClaudeEffort = "medium"
	// OpenRouterReasoningTokens is OpenRouter's thinking budget
	// (reasoning.max_tokens, Gemini's thinkingBudget). Thinking is never shown.
	OpenRouterReasoningTokens = 1024
	// OpenRouterAskReasoningEffort is the Ask model's thinking level
	// (reasoning.effort). DeepSeek V4.1 Flash 1017 supports max, high, low and
	// none, and defaults to high.
	OpenRouterAskReasoningEffort = "high"
	// OpenRouterAskWebResults is the most results one web search may return.
	// Ten is the most Exa includes in OpenRouter's base per-request price.
	// DeepSeek has no native search, so OpenRouter runs it on Exa.
	OpenRouterAskWebResults = 10
	// OpenRouterAskWebSearches caps how many searches one Ask request may run.
	// Two leaves room for a multi-hop question while keeping cost and latency
	// bounded even when the model wants to search repeatedly.
	OpenRouterAskWebSearches = 2
	// OpenRouterAskWebFetches caps how many full pages one Ask request may
	// open. Search finds the sources; two fetches let the model read the most
	// relevant ones without letting a confused loop crawl the web.
	OpenRouterAskWebFetches = 2
	// OpenRouterAskWebFetchTokens caps the text one fetch adds to the context.
	// 30k tokens is roughly 120 kB, enough for a long article or a small PDF;
	// it keeps two fetches from crowding out the picked page text and history.
	OpenRouterAskWebFetchTokens = 30000
	// ClaudeAskWebSearches caps Claude's direct web searches in one Ask turn.
	// A pause_turn continuation remains the same turn, so the API counts it
	// against this same budget.
	ClaudeAskWebSearches = 2
	// ClaudeAskWebFetches caps Claude's direct web fetches in one Ask turn.
	// Like searches, a pause_turn continuation remains the same turn.
	ClaudeAskWebFetches = 2
	// ClaudeAskWebFetchTokens caps the fetched text in Claude's context, for
	// the same reason as OpenRouter's cap.
	ClaudeAskWebFetchTokens = 30000
	// BraveAskImageSearches caps how many image searches the model may run in
	// one Ask round. Four leaves room for a multi-hop visual question while
	// keeping Brave requests bounded. The model still decides whether to use
	// them.
	BraveAskImageSearches = 4
)

// Load reads config from the environment. It also returns the missing API
// keys; the caller only warns, because each handler reports its own missing
// piece instead of the whole server failing.
//
// The error is for a configuration that must not run at all: more than one
// variable of the Memoir route set, neither ASSEMBLYAI_API_KEY nor
// DEEPGRAM_API_KEY set, a retired transcription variable still present
// (PARAKEET_URL, MAI_TRANSCRIBE_API_KEY, AZURE_SPEECH_KEY,
// AZURE_SPEECH_ENDPOINT, GROQ_API_KEY, GROQ_MODEL), or a Memoir Claude key
// with a model id that is not Claude's. The caller exits on it.
func Load() (Config, []string, error) {
	c := Config{
		ListenAddr:                 env("LISTEN_ADDR", "127.0.0.1:8770"),
		AssemblyAIKey:              os.Getenv("ASSEMBLYAI_API_KEY"),
		DeepgramAPIKey:             os.Getenv("DEEPGRAM_API_KEY"),
		OpenRouterTranscribeAPIKey: os.Getenv("OPENROUTER_TRANSCRIBE_API_KEY"),

		OpenRouterAPIKey: os.Getenv("OPENROUTER_API_KEY"),
		ClaudeAPIKey:     os.Getenv("CLAUDE_API_KEY"),

		AskClaudeAPIKey:     os.Getenv("ASK_CLAUDE_API_KEY"),
		AskOpenRouterAPIKey: os.Getenv("ASK_OPENROUTER_API_KEY"),
		BraveSearchAPIKey:   os.Getenv("BRAVE_SEARCH_API_KEY"),

		MemoirClaudeAPIKey: os.Getenv("MEMOIR_CLAUDE_API_KEY"),
		MemoirModel:        os.Getenv("MEMOIR_MODEL"),
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
	if err := c.resolveAsk(); err != nil {
		return c, nil, err
	}
	if err := c.resolveMemoir(); err != nil {
		return c, nil, err
	}

	var missing []string
	for _, k := range []struct{ name, val string }{
		{"OPENROUTER_API_KEY or CLAUDE_API_KEY", c.ExplainProvider},
		{"ASK_CLAUDE_API_KEY or ASK_OPENROUTER_API_KEY", c.AskProvider},
		{"MEMOIR_CLAUDE_API_KEY", c.MemoirProvider},
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

// resolveExplain collects the /api/explain providers. Any key may be set; the
// tray picker then switches between them. An empty key counts as unset, because
// .env.example ships empty lines. Models are fixed per provider.
func (c *Config) resolveExplain() error {
	if c.ClaudeAPIKey != "" {
		c.ExplainProviders = append(c.ExplainProviders, ProviderClaude)
	}
	if c.OpenRouterAPIKey != "" {
		c.ExplainProviders = append(c.ExplainProviders, ProviderOpenRouter)
	}
	if len(c.ExplainProviders) > 0 {
		c.ExplainProvider = c.ExplainProviders[0]
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

// resolveAsk collects the /api/ask providers. Every key may be set; the tray
// picker then switches between them. An empty key counts as unset, because
// .env.example ships empty lines. Models are fixed per provider.
func (c *Config) resolveAsk() error {
	if c.AskClaudeAPIKey != "" {
		c.AskProviders = append(c.AskProviders, ProviderClaude)
	}
	if c.AskOpenRouterAPIKey != "" {
		c.AskProviders = append(c.AskProviders, ProviderOpenRouter)
	}
	if len(c.AskProviders) > 0 {
		c.AskProvider = c.AskProviders[0]
	}
	return nil
}

// AskModelFor returns the fixed model id for provider.
func (c Config) AskModelFor(provider string) string {
	switch provider {
	case ProviderClaude:
		return defaultClaudeModel
	case ProviderOpenRouter:
		return defaultOpenRouterAskModel
	}
	return ""
}

// settleClaudeModel applies the default and check used by the Claude routes.
// keyVar and modelVar name the environment variables in errors.
func settleClaudeModel(model *string, keyVar, modelVar string) error {
	if *model == "" {
		*model = defaultClaudeModel
	}
	if !strings.HasPrefix(*model, "claude-") {
		return fmt.Errorf("%s is set but %s=%q is not a Claude model id "+
			"(they start with \"claude-\", for example %s). Delete the %s "+
			"line to use the default", keyVar, modelVar, *model, defaultClaudeModel, modelVar)
	}
	return nil
}

// resolveMemoir settles the Memoir provider key and model.
func (c *Config) resolveMemoir() error {
	if c.MemoirClaudeAPIKey == "" {
		return nil
	}
	c.MemoirProvider = ProviderClaude
	return settleClaudeModel(&c.MemoirModel, "MEMOIR_CLAUDE_API_KEY", "MEMOIR_MODEL")
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
			parts = append(parts, fmt.Sprintf("Claude Platform, model %s, effort %s", c.ExplainModelFor(p), ClaudeEffort))
		case ProviderOpenRouter:
			parts = append(parts, fmt.Sprintf("OpenRouter, model %s, %d thinking tokens", c.ExplainModelFor(p), OpenRouterReasoningTokens))
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

// AskRoute is one line saying where /api/ask goes, for the startup log.
func (c Config) AskRoute() string {
	var parts []string
	for _, p := range c.AskProviders {
		switch p {
		case ProviderClaude:
			parts = append(parts, fmt.Sprintf("Claude Platform, model %s, effort %s, web search + fetch on", c.AskModelFor(p), ClaudeEffort))
		case ProviderOpenRouter:
			parts = append(parts, fmt.Sprintf("OpenRouter, model %s, reasoning %s, web search + fetch on", c.AskModelFor(p), OpenRouterAskReasoningEffort))
		}
	}
	switch len(parts) {
	case 0:
		return "none (neither ASK_CLAUDE_API_KEY nor ASK_OPENROUTER_API_KEY is set)"
	case 1:
		return parts[0]
	}
	return strings.Join(parts, " and ") +
		fmt.Sprintf(" (%d choices; tray picker, starting on %s)", len(parts), askProviderName(c.AskProvider))
}

// askProviderName is the short human name used in the startup log.
func askProviderName(provider string) string {
	switch provider {
	case ProviderClaude:
		return "Claude Platform (search + fetch)"
	case ProviderOpenRouter:
		return "OpenRouter (search + fetch)"
	}
	return provider
}

// MemoirRoute is one line saying where /api/memoir/check goes, for the startup log.
func (c Config) MemoirRoute() string {
	if c.MemoirProvider == ProviderClaude {
		return fmt.Sprintf("Claude Platform, model %s, effort %s", c.MemoirModel, ClaudeEffort)
	}
	return "none (MEMOIR_CLAUDE_API_KEY is not set)"
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
