package config

import (
	"strings"
	"testing"
)

// clearTranscribeEnv blanks every variable that decides the /api/transcribe/stream
// route, so the machine's own environment cannot leak into a case. It then
// leaves ASSEMBLYAI_API_KEY set, because Load needs at least one transcribe
// provider; the transcribe cases override it. The retired GROQ_API_KEY,
// GROQ_MODEL, PARAKEET_URL, MAI_TRANSCRIBE_API_KEY, AZURE_SPEECH_KEY and
// AZURE_SPEECH_ENDPOINT are cleared too; cases set them to prove Load refuses.
func clearTranscribeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GROQ_API_KEY", "GROQ_MODEL", "OPENROUTER_TRANSCRIBE_API_KEY",
		"ASSEMBLYAI_API_KEY", "DEEPGRAM_API_KEY",
		"PARAKEET_URL", "MAI_TRANSCRIBE_API_KEY", "AZURE_SPEECH_KEY", "AZURE_SPEECH_ENDPOINT",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("ASSEMBLYAI_API_KEY", "test-assemblyai")
}

// clearExplainEnv blanks every variable that decides the explain route, so the
// machine's own environment cannot leak into a case.
func clearExplainEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OPENROUTER_API_KEY", "CLAUDE_API_KEY",
		"EXPLAIN_EFFORT", "EXPLAIN_REASONING_TOKENS",
	} {
		t.Setenv(k, "")
	}
}

// clearAskEnv blanks every variable that decides the ask route, so the
// machine's own environment cannot leak into a case.
func clearAskEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ASK_CLAUDE_API_KEY", "ASK_CEREBRAS_API_KEY", "ASK_OPENROUTER_API_KEY",
		"ASK_EFFORT", "ASK_REASONING_EFFORT",
	} {
		t.Setenv(k, "")
	}
}

// clearMemoirEnv does the same for the variables that decide the Memoir route.
// The retired CEREBRAS_API_KEY is in the list too: it would make Load refuse.
func clearMemoirEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MEMOIR_CEREBRAS_API_KEY", "MEMOIR_CLAUDE_API_KEY", "CEREBRAS_API_KEY", "MEMOIR_MODEL", "MEMOIR_EFFORT", "MEMOIR_REASONING_EFFORT"} {
		t.Setenv(k, "")
	}
}

func TestLoadExplainProvider(t *testing.T) {
	cases := []struct {
		name               string
		env                map[string]string
		wantProviders      []string
		wantProvider       string
		wantModel          string
		wantMissingExplain bool
		wantOK             bool
	}{
		{"neither key", nil, nil, "", "", true, true},
		{"openrouter", map[string]string{"OPENROUTER_API_KEY": "or"}, []string{ProviderOpenRouter}, ProviderOpenRouter, "google/gemini-3.8-flash", false, true},
		{"claude", map[string]string{"CLAUDE_API_KEY": "cl"}, []string{ProviderClaude}, ProviderClaude, "claude-opus-5-5", false, true},
		{"both", map[string]string{"OPENROUTER_API_KEY": "or", "CLAUDE_API_KEY": "cl"}, []string{ProviderClaude, ProviderOpenRouter}, ProviderClaude, "claude-opus-5-5", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTranscribeEnv(t)
			clearExplainEnv(t)
			clearMemoirEnv(t)
			clearAskEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, missing, err := Load()
			if (err == nil) != tc.wantOK {
				t.Fatalf("err %v, want ok=%v", err, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if len(c.ExplainProviders) != len(tc.wantProviders) {
				t.Fatalf("providers %v, want %v", c.ExplainProviders, tc.wantProviders)
			}
			for i := range tc.wantProviders {
				if c.ExplainProviders[i] != tc.wantProviders[i] {
					t.Fatalf("providers %v, want %v", c.ExplainProviders, tc.wantProviders)
				}
			}
			if c.ExplainProvider != tc.wantProvider || c.ExplainModelFor(c.ExplainProvider) != tc.wantModel {
				t.Errorf("got provider=%q model=%q; want %q %q",
					c.ExplainProvider, c.ExplainModelFor(c.ExplainProvider),
					tc.wantProvider, tc.wantModel)
			}
			gotMissing := false
			for _, m := range missing {
				if strings.HasPrefix(m, "OPENROUTER_API_KEY or") {
					gotMissing = true
				}
			}
			if gotMissing != tc.wantMissingExplain {
				t.Errorf("missing %v, want explain missing=%v", missing, tc.wantMissingExplain)
			}
		})
	}
}

func TestExplainRouteNamesTheProviderAndModel(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearMemoirEnv(t)
	clearAskEnv(t)
	t.Setenv("CLAUDE_API_KEY", "cl")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ExplainRoute(); !strings.Contains(got, "Claude") || !strings.Contains(got, "claude-opus-5-5") || !strings.Contains(got, "medium") {
		t.Errorf("route %q", got)
	}

	t.Setenv("OPENROUTER_API_KEY", "or")
	c, _, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	got := c.ExplainRoute()
	for _, want := range []string{"Claude", "OpenRouter", "2 providers", "tray picker", "starting on Claude Platform"} {
		if !strings.Contains(got, want) {
			t.Errorf("route %q missing %q", got, want)
		}
	}
}

// Thinking depth is fixed in code, like the models. Leftover tuning variables
// from older installs must be ignored rather than change a route or stop the
// service.
func TestThinkingControlsAreFixed(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearAskEnv(t)
	clearMemoirEnv(t)
	for k, v := range map[string]string{
		"EXPLAIN_REASONING_TOKENS": "999999",
		"EXPLAIN_EFFORT":           "turbo",
		"ASK_EFFORT":               "low",
		"ASK_REASONING_EFFORT":     "none",
		"MEMOIR_EFFORT":            "max",
		"MEMOIR_REASONING_EFFORT":  "low",
		"OPENROUTER_API_KEY":       "or",
		"CLAUDE_API_KEY":           "cl",
		"ASK_CLAUDE_API_KEY":       "cl",
		"ASK_CEREBRAS_API_KEY":     "ce",
		"ASK_OPENROUTER_API_KEY":   "or",
		"MEMOIR_CLAUDE_API_KEY":    "cl",
	} {
		t.Setenv(k, v)
	}
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ExplainRoute(); !strings.Contains(got, "effort medium") || !strings.Contains(got, "1024 thinking tokens") {
		t.Errorf("explain route %q does not use the fixed thinking controls", got)
	}
	if got := c.AskRoute(); !strings.Contains(got, "effort medium") || !strings.Contains(got, "reasoning medium") ||
		!strings.Contains(got, "reasoning high") || !strings.Contains(got, "web search + fetch on") {
		t.Errorf("ask route %q does not use the fixed thinking controls", got)
	}
	if got := c.MemoirRoute(); !strings.Contains(got, "effort medium") {
		t.Errorf("memoir route %q does not use the fixed thinking controls", got)
	}
}

func TestLoadAskProvider(t *testing.T) {
	cases := []struct {
		name           string
		env            map[string]string
		wantProviders  []string
		wantProvider   string
		wantModel      string
		wantMissingAsk bool
		wantOK         bool
	}{
		{"no key", nil, nil, "", "", true, true},
		{"claude", map[string]string{"ASK_CLAUDE_API_KEY": "cl"}, []string{ProviderClaude, ProviderClaudeNoSearch}, ProviderClaude, "claude-opus-5-5", false, true},
		{"cerebras", map[string]string{"ASK_CEREBRAS_API_KEY": "ce"}, []string{ProviderCerebras}, ProviderCerebras, "qwen-3.8-27b", false, true},
		{"openrouter", map[string]string{"ASK_OPENROUTER_API_KEY": "or"}, []string{ProviderOpenRouter, ProviderOpenRouterNoSearch}, ProviderOpenRouter, "deepseek/deepseek-v4.1-flash-1017", false, true},
		{"claude and cerebras", map[string]string{"ASK_CLAUDE_API_KEY": "cl", "ASK_CEREBRAS_API_KEY": "ce"}, []string{ProviderClaude, ProviderClaudeNoSearch, ProviderCerebras}, ProviderClaude, "claude-opus-5-5", false, true},
		{"all three", map[string]string{"ASK_CLAUDE_API_KEY": "cl", "ASK_CEREBRAS_API_KEY": "ce", "ASK_OPENROUTER_API_KEY": "or"}, []string{ProviderClaude, ProviderClaudeNoSearch, ProviderCerebras, ProviderOpenRouter, ProviderOpenRouterNoSearch}, ProviderClaude, "claude-opus-5-5", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTranscribeEnv(t)
			clearExplainEnv(t)
			clearAskEnv(t)
			clearMemoirEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, missing, err := Load()
			if (err == nil) != tc.wantOK {
				t.Fatalf("err %v, want ok=%v", err, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if len(c.AskProviders) != len(tc.wantProviders) {
				t.Fatalf("providers %v, want %v", c.AskProviders, tc.wantProviders)
			}
			for i := range tc.wantProviders {
				if c.AskProviders[i] != tc.wantProviders[i] {
					t.Fatalf("providers %v, want %v", c.AskProviders, tc.wantProviders)
				}
			}
			if c.AskProvider != tc.wantProvider || c.AskModelFor(c.AskProvider) != tc.wantModel {
				t.Errorf("got provider=%q model=%q; want %q %q",
					c.AskProvider, c.AskModelFor(c.AskProvider),
					tc.wantProvider, tc.wantModel)
			}
			if got := c.AskModelFor(ProviderClaudeNoSearch); got != "claude-opus-5-5" {
				t.Errorf("Claude no-search model %q", got)
			}
			if got := c.AskModelFor(ProviderOpenRouterNoSearch); got != "deepseek/deepseek-v4.1-flash-1017" {
				t.Errorf("OpenRouter no-search model %q", got)
			}
			gotMissing := false
			for _, m := range missing {
				if strings.HasPrefix(m, "ASK_CLAUDE_API_KEY") {
					gotMissing = true
				}
			}
			if gotMissing != tc.wantMissingAsk {
				t.Errorf("missing %v, want ask missing=%v", missing, tc.wantMissingAsk)
			}
		})
	}
}

func TestAskKeysAreIndependentOfExplainKeys(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearAskEnv(t)
	clearMemoirEnv(t)
	t.Setenv("CLAUDE_API_KEY", "explain-claude")
	c, _, err := Load()
	if err != nil || c.ExplainProvider != ProviderClaude || c.AskProvider != "" {
		t.Fatalf("explain %q ask %q err %v", c.ExplainProvider, c.AskProvider, err)
	}
	t.Setenv("ASK_CEREBRAS_API_KEY", "ask-cerebras")
	c, _, err = Load()
	if err != nil || c.AskProvider != ProviderCerebras || c.AskProviders[0] != ProviderCerebras {
		t.Fatalf("ask %q providers %v err %v", c.AskProvider, c.AskProviders, err)
	}
	// The explain OpenRouter key must not select OpenRouter for Ask.
	t.Setenv("OPENROUTER_API_KEY", "explain-openrouter")
	c, _, err = Load()
	if err != nil || c.AskProvider != ProviderCerebras || len(c.AskProviders) != 1 {
		t.Fatalf("ask %q providers %v err %v after setting only the explain OpenRouter key",
			c.AskProvider, c.AskProviders, err)
	}
	t.Setenv("ASK_OPENROUTER_API_KEY", "ask-openrouter")
	c, _, err = Load()
	if err != nil || len(c.AskProviders) != 3 || c.AskProviders[1] != ProviderOpenRouter ||
		c.AskProviders[2] != ProviderOpenRouterNoSearch {
		t.Fatalf("ask %q providers %v err %v after adding the ask OpenRouter key",
			c.AskProvider, c.AskProviders, err)
	}
}

func TestAskRouteNamesTheProviderAndModel(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearAskEnv(t)
	clearMemoirEnv(t)
	t.Setenv("ASK_CLAUDE_API_KEY", "cl")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.AskRoute(); !strings.Contains(got, "Claude") || !strings.Contains(got, "claude-opus-5-5") ||
		!strings.Contains(got, "medium") || !strings.Contains(got, "web search + fetch on") {
		t.Errorf("route %q", got)
	}

	t.Setenv("ASK_CEREBRAS_API_KEY", "ce")
	c, _, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	got := c.AskRoute()
	for _, want := range []string{"Claude", "Cerebras", "qwen-3.8-27b", "3 choices", "tray picker", "starting on Claude Platform", "web search + fetch off"} {
		if !strings.Contains(got, want) {
			t.Errorf("route %q missing %q", got, want)
		}
	}

	t.Setenv("ASK_OPENROUTER_API_KEY", "or")
	c, _, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	got = c.AskRoute()
	for _, want := range []string{"OpenRouter", "deepseek/deepseek-v4.1-flash-1017", "reasoning high", "web search + fetch on", "web search + fetch off", "5 choices"} {
		if !strings.Contains(got, want) {
			t.Errorf("route %q missing %q", got, want)
		}
	}

	if got := (Config{}).AskRoute(); !strings.Contains(got, "ASK_CLAUDE_API_KEY") ||
		!strings.Contains(got, "ASK_CEREBRAS_API_KEY") || !strings.Contains(got, "ASK_OPENROUTER_API_KEY") {
		t.Errorf("unconfigured route %q does not name all three keys", got)
	}
}

func TestLoadRefusesBothMemoirKeys(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearMemoirEnv(t)
	clearAskEnv(t)
	t.Setenv("MEMOIR_CEREBRAS_API_KEY", "ce")
	t.Setenv("MEMOIR_CLAUDE_API_KEY", "cl")
	_, _, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MEMOIR_CEREBRAS_API_KEY") || !strings.Contains(err.Error(), "MEMOIR_CLAUDE_API_KEY") {
		t.Fatalf("err %v, want one naming both keys", err)
	}
}

func TestLoadMemoirProvider(t *testing.T) {
	cases := []struct {
		name                      string
		env                       map[string]string
		wantProvider, wantModel   string
		wantMissingMemoir, wantOK bool
	}{
		{"neither key", nil, "", "", true, true},
		{"cerebras default model", map[string]string{"MEMOIR_CEREBRAS_API_KEY": "ce"}, ProviderCerebras, "qwen-3.8-27b", false, true},
		{"cerebras custom model", map[string]string{"MEMOIR_CEREBRAS_API_KEY": "ce", "MEMOIR_MODEL": "llama-9"}, ProviderCerebras, "llama-9", false, true},
		{"claude defaults", map[string]string{"MEMOIR_CLAUDE_API_KEY": "cl"}, ProviderClaude, "claude-opus-5-5", false, true},
		{"claude custom", map[string]string{"MEMOIR_CLAUDE_API_KEY": "cl", "MEMOIR_MODEL": "claude-sonnet-5-5"}, ProviderClaude, "claude-sonnet-5-5", false, true},
		{"claude with a cerebras model id", map[string]string{"MEMOIR_CLAUDE_API_KEY": "cl", "MEMOIR_MODEL": "qwen-3.8-27b"}, "", "", false, false},
		{"the retired CEREBRAS_API_KEY", map[string]string{"CEREBRAS_API_KEY": "ce", "MEMOIR_CLAUDE_API_KEY": "cl"}, "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTranscribeEnv(t)
			clearExplainEnv(t)
			clearMemoirEnv(t)
			clearAskEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, missing, err := Load()
			if (err == nil) != tc.wantOK {
				t.Fatalf("err %v, want ok=%v", err, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if c.MemoirProvider != tc.wantProvider || c.MemoirModel != tc.wantModel {
				t.Errorf("got %q %q, want %q %q", c.MemoirProvider, c.MemoirModel, tc.wantProvider, tc.wantModel)
			}
			gotMissing := false
			for _, m := range missing {
				if strings.Contains(m, "MEMOIR_CLAUDE_API_KEY") {
					gotMissing = true
				}
			}
			if gotMissing != tc.wantMissingMemoir {
				t.Errorf("missing %v, want memoir missing=%v", missing, tc.wantMissingMemoir)
			}
		})
	}
}

// The plain CLAUDE_API_KEY belongs to /api/explain alone: it must not select
// Claude for Memoir, and an empty MEMOIR_ line counts as unset.
func TestMemoirKeysAreIndependentOfExplainKeys(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearMemoirEnv(t)
	clearAskEnv(t)
	t.Setenv("CLAUDE_API_KEY", "cl")
	c, _, err := Load()
	if err != nil || c.MemoirProvider != "" || c.ExplainProvider != ProviderClaude {
		t.Fatalf("memoir %q explain %q err %v", c.MemoirProvider, c.ExplainProvider, err)
	}
	t.Setenv("MEMOIR_CEREBRAS_API_KEY", "ce")
	t.Setenv("MEMOIR_CLAUDE_API_KEY", "")
	c, _, err = Load()
	if err != nil || c.MemoirProvider != ProviderCerebras {
		t.Fatalf("memoir %q err %v", c.MemoirProvider, err)
	}
}

func TestMemoirRouteNamesTheProviderAndModel(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearMemoirEnv(t)
	clearAskEnv(t)
	t.Setenv("MEMOIR_CLAUDE_API_KEY", "cl")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MemoirRoute(); !strings.Contains(got, "Claude") || !strings.Contains(got, "claude-opus-5-5") || !strings.Contains(got, "medium") {
		t.Errorf("route %q", got)
	}
}

func TestLoadKeepsVerbatimKeyAlongsideAssemblyAI(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearMemoirEnv(t)
	clearAskEnv(t)
	t.Setenv("ASSEMBLYAI_API_KEY", "aai")
	t.Setenv("OPENROUTER_TRANSCRIBE_API_KEY", "or")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AssemblyAIKey != "aai" || c.OpenRouterTranscribeAPIKey != "or" {
		t.Errorf("transcription keys were not kept: assemblyai %q, verbatim %q", c.AssemblyAIKey, c.OpenRouterTranscribeAPIKey)
	}
	if len(c.TranscribeProviders) != 1 || c.TranscribeProviders[0] != ProviderAssemblyAI || c.TranscribeProvider != ProviderAssemblyAI {
		t.Errorf("providers %v, starting %q; want only %q", c.TranscribeProviders, c.TranscribeProvider, ProviderAssemblyAI)
	}
}

func TestLoadRequiresTranscribeProvider(t *testing.T) {
	cases := []struct {
		name          string
		env           map[string]string
		wantProviders []string
		wantOK        bool
	}{
		{"none set", map[string]string{"ASSEMBLYAI_API_KEY": ""}, nil, false},
		{"only the verbatim key", map[string]string{"ASSEMBLYAI_API_KEY": "", "OPENROUTER_TRANSCRIBE_API_KEY": "or"}, nil, false},
		{"assemblyai", map[string]string{"ASSEMBLYAI_API_KEY": "aai"}, []string{ProviderAssemblyAI}, true},
		{"deepgram", map[string]string{"ASSEMBLYAI_API_KEY": "", "DEEPGRAM_API_KEY": "dg"}, []string{ProviderDeepgram}, true},
		{"both", map[string]string{"ASSEMBLYAI_API_KEY": "aai", "DEEPGRAM_API_KEY": "dg"}, []string{ProviderAssemblyAI, ProviderDeepgram}, true},
		{"assemblyai and verbatim", map[string]string{"ASSEMBLYAI_API_KEY": "aai", "OPENROUTER_TRANSCRIBE_API_KEY": "or"}, []string{ProviderAssemblyAI}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTranscribeEnv(t)
			clearExplainEnv(t)
			clearMemoirEnv(t)
			clearAskEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, _, err := Load()
			if (err == nil) != tc.wantOK {
				t.Fatalf("err %v, want ok=%v", err, tc.wantOK)
			}
			if !tc.wantOK {
				if err == nil || !strings.Contains(err.Error(), "ASSEMBLYAI_API_KEY") ||
					!strings.Contains(err.Error(), "DEEPGRAM_API_KEY") {
					t.Fatalf("err %v, want one naming both keys", err)
				}
				return
			}
			if len(c.TranscribeProviders) != len(tc.wantProviders) {
				t.Fatalf("providers %v, want %v", c.TranscribeProviders, tc.wantProviders)
			}
			for i := range tc.wantProviders {
				if c.TranscribeProviders[i] != tc.wantProviders[i] {
					t.Fatalf("providers %v, want %v", c.TranscribeProviders, tc.wantProviders)
				}
			}
			if c.TranscribeProvider != tc.wantProviders[0] {
				t.Errorf("starting provider %q, want %q", c.TranscribeProvider, tc.wantProviders[0])
			}
		})
	}
}

func TestLoadRefusesRetiredParakeetURL(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearMemoirEnv(t)
	clearAskEnv(t)
	t.Setenv("PARAKEET_URL", "http://127.0.0.1:8790")
	_, _, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PARAKEET_URL") || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("err %v, want one saying PARAKEET_URL is no longer supported", err)
	}
}

func TestLoadRefusesRetiredGroqVariables(t *testing.T) {
	for _, key := range []string{"GROQ_API_KEY", "GROQ_MODEL"} {
		t.Run(key, func(t *testing.T) {
			clearTranscribeEnv(t)
			clearExplainEnv(t)
			clearMemoirEnv(t)
			clearAskEnv(t)
			t.Setenv(key, "old-value")
			_, _, err := Load()
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "no longer used") {
				t.Fatalf("err %v, want one naming retired %s", err, key)
			}
		})
	}
}

func TestLoadRefusesRetiredAzureVariables(t *testing.T) {
	for _, key := range []string{"MAI_TRANSCRIBE_API_KEY", "AZURE_SPEECH_KEY", "AZURE_SPEECH_ENDPOINT"} {
		t.Run(key, func(t *testing.T) {
			clearTranscribeEnv(t)
			clearExplainEnv(t)
			clearMemoirEnv(t)
			clearAskEnv(t)
			t.Setenv(key, "old-value")
			_, _, err := Load()
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "no longer used") {
				t.Fatalf("err %v, want one naming retired %s", err, key)
			}
		})
	}
}

// OPENROUTER_API_KEY belongs to /api/explain; /api/transcribe-verbatim uses the
// separate OPENROUTER_TRANSCRIBE_API_KEY. One must not select the other.
func TestOpenRouterKeysAreIndependent(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearMemoirEnv(t)
	clearAskEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "explain-key")
	c, _, err := Load()
	if err != nil || c.AssemblyAIKey != "test-assemblyai" || c.OpenRouterTranscribeAPIKey != "" {
		t.Fatalf("assemblyai %q verbatim %q err %v", c.AssemblyAIKey, c.OpenRouterTranscribeAPIKey, err)
	}
	if c.ExplainProvider != ProviderOpenRouter {
		t.Errorf("explain provider %q, want %q", c.ExplainProvider, ProviderOpenRouter)
	}
	t.Setenv("OPENROUTER_TRANSCRIBE_API_KEY", "stt-key")
	c, _, err = Load()
	if err != nil || c.OpenRouterTranscribeAPIKey != "stt-key" || c.ExplainProvider != ProviderOpenRouter {
		t.Fatalf("verbatim %q explain %q err %v", c.OpenRouterTranscribeAPIKey, c.ExplainProvider, err)
	}
}

func TestTranscribeRouteNamesTheProviderAndModel(t *testing.T) {
	clearTranscribeEnv(t)
	clearExplainEnv(t)
	clearMemoirEnv(t)
	clearAskEnv(t)
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := c.TranscribeRoute()
	for _, w := range []string{"AssemblyAI", "universal-3-5-pro"} {
		if !strings.Contains(got, w) {
			t.Errorf("route %q missing %q", got, w)
		}
	}

	t.Setenv("DEEPGRAM_API_KEY", "dg")
	c, _, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	got = c.TranscribeRoute()
	for _, w := range []string{"AssemblyAI", "Deepgram", "nova-3", "2 providers", "tray picker", "starting on AssemblyAI Universal-3.5 Pro"} {
		if !strings.Contains(got, w) {
			t.Errorf("route %q missing %q", got, w)
		}
	}

	if got := (Config{}).TranscribeRoute(); !strings.Contains(got, "ASSEMBLYAI_API_KEY") ||
		!strings.Contains(got, "DEEPGRAM_API_KEY") {
		t.Errorf("unconfigured route %q does not name both keys", got)
	}
}
