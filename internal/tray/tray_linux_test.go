//go:build linux

package tray

import (
	"testing"

	"ai-service/internal/config"
)

func TestLinuxMenuHasTwoSections(t *testing.T) {
	c := &fakeController{
		transcribe: []string{config.ProviderAssemblyAI, config.ProviderDeepgram},
		explain:    []string{config.ProviderClaude, config.ProviderOpenRouter},
		currentT:   config.ProviderAssemblyAI,
		currentE:   config.ProviderClaude,
	}
	var picked []string
	m := newMenu(c, func(kind, provider string) {
		picked = append(picked, kind+"/"+provider)
	})

	root := m.buildLocked()
	if len(root.V2) != 6 {
		t.Fatalf("menu has %d children, want 2 headers and 4 items", len(root.V2))
	}
	wantLabels := []string{
		"Transcribe with", "AssemblyAI Universal-3.5 Pro", "Deepgram Nova-3",
		"Explain with", "Claude Platform Opus 5.5", "OpenRouter Gemini 3.8 Flash",
	}
	for i, want := range wantLabels {
		child := root.V2[i].Value().(layout)
		if got := child.V1["label"].Value().(string); got != want {
			t.Errorf("child %d label %q, want %q", i, got, want)
		}
	}

	// id 5 is Claude (already current), id 6 is OpenRouter, id 3 is Deepgram.
	m.pick(5)
	if len(picked) != 0 {
		t.Fatalf("picking the current provider called back: %v", picked)
	}
	m.pick(6)
	if c.currentE != config.ProviderOpenRouter {
		t.Fatalf("explain provider %q, want %q", c.currentE, config.ProviderOpenRouter)
	}
	m.pick(3)
	if c.currentT != config.ProviderDeepgram {
		t.Fatalf("transcribe provider %q, want %q", c.currentT, config.ProviderDeepgram)
	}
	if len(picked) != 2 || picked[0] != PickExplain+"/"+config.ProviderOpenRouter ||
		picked[1] != PickTranscribe+"/"+config.ProviderDeepgram {
		t.Fatalf("picks %v", picked)
	}
}
