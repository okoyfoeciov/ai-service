package tray

import (
	"testing"

	"ai-service/internal/config"
)

type fakeController struct {
	transcribe, explain, ask, memoir       []string
	currentT, currentE, currentA, currentM string
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

func (f *fakeController) MemoirProviders() []string { return f.memoir }
func (f *fakeController) MemoirProvider() string    { return f.currentM }
func (f *fakeController) SetMemoirProvider(p string) bool {
	for _, v := range f.memoir {
		if v == p {
			f.currentM = p
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
		memoir     []string
		want       []string
	}{
		{"one each", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude}, []string{config.ProviderClaude}, []string{config.ProviderClaude}, nil},
		{"two transcribe", []string{config.ProviderAssemblyAI, config.ProviderDeepgram}, []string{config.ProviderClaude}, []string{config.ProviderClaude}, nil, []string{PickTranscribe}},
		{"two explain", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude, config.ProviderOpenRouter}, []string{config.ProviderClaude}, nil, []string{PickExplain}},
		{"two ask choices", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude}, []string{config.ProviderClaude, config.ProviderOpenRouter}, nil, []string{PickAsk}},
		{"two memoir choices", []string{config.ProviderAssemblyAI}, []string{config.ProviderClaude}, []string{config.ProviderClaude}, []string{config.ProviderClaude, config.ProviderOpenRouter}, []string{PickMemoir}},
		{"two each", []string{config.ProviderAssemblyAI, config.ProviderDeepgram}, []string{config.ProviderClaude, config.ProviderOpenRouter}, []string{config.ProviderClaude, config.ProviderOpenRouter}, []string{config.ProviderClaude, config.ProviderOpenRouter}, []string{PickTranscribe, PickExplain, PickAsk, PickMemoir}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeController{transcribe: tc.transcribe, explain: tc.explain, ask: tc.ask, memoir: tc.memoir}
			if len(tc.transcribe) > 0 {
				c.currentT = tc.transcribe[0]
			}
			if len(tc.explain) > 0 {
				c.currentE = tc.explain[0]
			}
			if len(tc.ask) > 0 {
				c.currentA = tc.ask[0]
			}
			if len(tc.memoir) > 0 {
				c.currentM = tc.memoir[0]
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
		{PickExplain, config.ProviderClaude, "Claude Platform Opus 5.5 (thinking medium)"},
		{PickExplain, config.ProviderOpenRouter, "OpenRouter GPT-6.2 Sol (fast, thinking low)"},
		{PickExplain, config.ProviderOpenRouterKimi, "OpenRouter Kimi K3.1 Pro (thinking off)"},
		{PickAsk, config.ProviderClaude, "Claude Platform Opus 5.5 (search + fetch, thinking medium)"},
		{PickAsk, config.ProviderOpenRouter, "OpenRouter GPT-6.2 Sol (search + fetch, thinking medium)"},
		{PickMemoir, config.ProviderClaude, "Claude Platform (thinking medium)"},
		{PickMemoir, config.ProviderOpenRouter, "OpenRouter GPT-6.1 Terra (flex, thinking low)"},
	}
	for _, tc := range cases {
		if got := providerLabel(tc.kind, tc.provider); got != tc.want {
			t.Errorf("%s/%s: got %q, want %q", tc.kind, tc.provider, got, tc.want)
		}
	}
}

// A Memoir pick flips the Memoir provider and nothing else; a provider without
// a key is refused.
func TestSetProviderMemoir(t *testing.T) {
	c := &fakeController{
		explain: []string{config.ProviderClaude, config.ProviderOpenRouter}, currentE: config.ProviderClaude,
		memoir: []string{config.ProviderClaude, config.ProviderOpenRouter}, currentM: config.ProviderClaude,
	}
	if !setProvider(c, PickMemoir, config.ProviderOpenRouter) || c.currentM != config.ProviderOpenRouter || c.currentE != config.ProviderClaude {
		t.Fatalf("memoir %q explain %q", c.currentM, c.currentE)
	}
	if setProvider(c, PickMemoir, config.ProviderOpenRouterKimi) {
		t.Error("selected a Memoir provider that has no key")
	}
}
