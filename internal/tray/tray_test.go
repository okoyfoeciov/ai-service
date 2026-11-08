package tray

import (
	"testing"

	"ai-service/internal/config"
)

type fakeController struct {
	transcribe, explain, ask     []string
	currentT, currentE, currentA string
}

func (f *fakeController) TranscribeProviders() []string { return f.transcribe }
func (f *fakeController) TranscribeProvider() string    { return f.currentT }
func (f *fakeController) SetTranscribeProvider(p string) bool {
	for _, v := range f.transcribe {
		if v == p {
			f.currentT = p
			return true
		}
	}
	return false
}
func (f *fakeController) ExplainProviders() []string { return f.explain }
func (f *fakeController) ExplainProvider() string    { return f.currentE }
func (f *fakeController) SetExplainProvider(p string) bool {
	for _, v := range f.explain {
		if v == p {
			f.currentE = p
			return true
		}
	}
	return false
}
func (f *fakeController) AskProviders() []string { return f.ask }
func (f *fakeController) AskProvider() string    { return f.currentA }
func (f *fakeController) SetAskProvider(p string) bool {
	for _, v := range f.ask {
		if v == p {
			f.currentA = p
			return true
		}
	}
	return false
}

func TestPickerSectionsAreConditional(t *testing.T) {
	cases := []struct {
		name       string
		transcribe []string
		explain    []string
		ask        []string
		want       []string
	}{
		{"one each", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude}, []string{config.ProviderClaude}, nil},
		{"two transcribe", []string{config.ProviderAssemblyAI, config.ProviderDeepgram}, []string{config.ProviderClaude}, []string{config.ProviderClaude}, []string{PickTranscribe}},
		{"two explain", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude, config.ProviderOpenRouter}, []string{config.ProviderClaude}, []string{PickExplain}},
		{"two ask", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude}, []string{config.ProviderClaude, config.ProviderCerebras}, []string{PickAsk}},
		{"three ask", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude}, []string{config.ProviderClaude, config.ProviderCerebras, config.ProviderOpenRouter}, []string{PickAsk}},
		{"five ask choices", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude}, []string{
			config.ProviderClaude, config.ProviderClaudeNoSearch, config.ProviderCerebras,
			config.ProviderOpenRouter, config.ProviderOpenRouterNoSearch,
		}, []string{PickAsk}},
		{"two each", []string{config.ProviderAssemblyAI, config.ProviderDeepgram}, []string{config.ProviderClaude, config.ProviderOpenRouter}, []string{config.ProviderClaude, config.ProviderCerebras}, []string{PickTranscribe, PickExplain, PickAsk}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeController{transcribe: tc.transcribe, explain: tc.explain, ask: tc.ask}
			if len(tc.transcribe) > 0 {
				c.currentT = tc.transcribe[0]
			}
			if len(tc.explain) > 0 {
				c.currentE = tc.explain[0]
			}
			if len(tc.ask) > 0 {
				c.currentA = tc.ask[0]
			}
			sections := pickerSections(c)
			if len(sections) != len(tc.want) {
				t.Fatalf("sections %v, want %v", sections, tc.want)
			}
			for i, want := range tc.want {
				if sections[i].kind != want {
					t.Errorf("section %d kind %q, want %q", i, sections[i].kind, want)
				}
			}
		})
	}
}

func TestProviderLabels(t *testing.T) {
	cases := []struct{ kind, provider, want string }{
		{PickTranscribe, config.ProviderAssemblyAI, "AssemblyAI Universal-3.5 Pro"},
		{PickTranscribe, config.ProviderDeepgram, "Deepgram Nova-3"},
		{PickExplain, config.ProviderClaude, "Claude Platform Opus 5.5"},
		{PickExplain, config.ProviderOpenRouter, "OpenRouter Gemini 3.8 Flash"},
		{PickAsk, config.ProviderClaude, "Claude Platform Opus 5.5 (search)"},
		{PickAsk, config.ProviderClaudeNoSearch, "Claude Platform Opus 5.5"},
		{PickAsk, config.ProviderCerebras, "Cerebras Qwen 3.8 27B"},
		{PickAsk, config.ProviderOpenRouter, "OpenRouter DeepSeek V4.1 Flash 1017 (search)"},
		{PickAsk, config.ProviderOpenRouterNoSearch, "OpenRouter DeepSeek V4.1 Flash 1017"},
	}
	for _, tc := range cases {
		if got := providerLabel(tc.kind, tc.provider); got != tc.want {
			t.Errorf("%s/%s: got %q, want %q", tc.kind, tc.provider, got, tc.want)
		}
	}
}
