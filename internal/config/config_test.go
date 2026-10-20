package config

import (
	"strings"
	"testing"
)

// clearExplainEnv blanks every variable that decides the explain route, so the
// machine's own environment cannot leak into a case.
func clearExplainEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OPENROUTER_API_KEY", "CLAUDE_API_KEY", "EXPLAIN_MODEL", "EXPLAIN_EFFORT", "EXPLAIN_REASONING_TOKENS"} {
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

func TestLoadRefusesBothExplainKeys(t *testing.T) {
	clearExplainEnv(t)
	clearMemoirEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "or")
	t.Setenv("CLAUDE_API_KEY", "cl")
	_, _, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") || !strings.Contains(err.Error(), "CLAUDE_API_KEY") {
		t.Fatalf("err %v, want one naming both keys", err)
	}
}

func TestLoadExplainProvider(t *testing.T) {
	cases := []struct {
		name                       string
		env                        map[string]string
		wantProvider, wantModel    string
		wantEffort                 string
		wantMissingExplain, wantOK bool
	}{
		{"neither key", nil, "", "google/gemini-3.8-flash", "", true, true},
		{"openrouter default model", map[string]string{"OPENROUTER_API_KEY": "or"}, ProviderOpenRouter, "google/gemini-3.8-flash", "", false, true},
		{"openrouter custom model", map[string]string{"OPENROUTER_API_KEY": "or", "EXPLAIN_MODEL": "anthropic/claude-opus-5-5"}, ProviderOpenRouter, "anthropic/claude-opus-5-5", "", false, true},
		{"claude defaults", map[string]string{"CLAUDE_API_KEY": "cl"}, ProviderClaude, "claude-opus-5-5", "medium", false, true},
		{"claude custom", map[string]string{"CLAUDE_API_KEY": "cl", "EXPLAIN_MODEL": "claude-sonnet-5-5", "EXPLAIN_EFFORT": "High"}, ProviderClaude, "claude-sonnet-5-5", "high", false, true},
		{"claude with an openrouter model id", map[string]string{"CLAUDE_API_KEY": "cl", "EXPLAIN_MODEL": "google/gemini-3.8-flash"}, "", "", "", false, false},
		{"claude with anthropic/ prefix", map[string]string{"CLAUDE_API_KEY": "cl", "EXPLAIN_MODEL": "anthropic/claude-opus-5-5"}, "", "", "", false, false},
		{"claude with bad effort", map[string]string{"CLAUDE_API_KEY": "cl", "EXPLAIN_EFFORT": "turbo"}, "", "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearExplainEnv(t)
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
			if c.ExplainProvider != tc.wantProvider || c.ExplainModel != tc.wantModel || c.ExplainEffort != tc.wantEffort {
				t.Errorf("got %q %q %q, want %q %q %q", c.ExplainProvider, c.ExplainModel, c.ExplainEffort, tc.wantProvider, tc.wantModel, tc.wantEffort)
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

// .env.example ships empty KEY= lines, so an empty value must count as unset.
func TestLoadEmptyKeysCountAsUnset(t *testing.T) {
	clearExplainEnv(t)
	clearMemoirEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "or")
	t.Setenv("CLAUDE_API_KEY", "")
	c, _, err := Load()
	if err != nil || c.ExplainProvider != ProviderOpenRouter {
		t.Fatalf("got %q, %v", c.ExplainProvider, err)
	}
}

func TestExplainRouteNamesTheProviderAndModel(t *testing.T) {
	clearExplainEnv(t)
	clearMemoirEnv(t)
	t.Setenv("CLAUDE_API_KEY", "cl")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ExplainRoute(); !strings.Contains(got, "Claude") || !strings.Contains(got, "claude-opus-5-5") || !strings.Contains(got, "medium") {
		t.Errorf("route %q", got)
	}
}

func TestLoadRefusesBothMemoirKeys(t *testing.T) {
	clearExplainEnv(t)
	clearMemoirEnv(t)
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
		wantEffort                string
		wantMissingMemoir, wantOK bool
	}{
		{"neither key", nil, "", "claude-opus-5-5", "", true, true},
		{"cerebras default model", map[string]string{"MEMOIR_CEREBRAS_API_KEY": "ce"}, ProviderCerebras, "qwen-3.8-27b", "", false, true},
		{"cerebras custom model", map[string]string{"MEMOIR_CEREBRAS_API_KEY": "ce", "MEMOIR_MODEL": "llama-9"}, ProviderCerebras, "llama-9", "", false, true},
		{"claude defaults", map[string]string{"MEMOIR_CLAUDE_API_KEY": "cl"}, ProviderClaude, "claude-opus-5-5", "medium", false, true},
		{"claude custom", map[string]string{"MEMOIR_CLAUDE_API_KEY": "cl", "MEMOIR_MODEL": "claude-sonnet-5-5", "MEMOIR_EFFORT": "High"}, ProviderClaude, "claude-sonnet-5-5", "high", false, true},
		{"claude with a cerebras model id", map[string]string{"MEMOIR_CLAUDE_API_KEY": "cl", "MEMOIR_MODEL": "qwen-3.8-27b"}, "", "", "", false, false},
		{"claude with bad effort", map[string]string{"MEMOIR_CLAUDE_API_KEY": "cl", "MEMOIR_EFFORT": "none"}, "", "", "", false, false},
		{"the retired CEREBRAS_API_KEY", map[string]string{"CEREBRAS_API_KEY": "ce", "MEMOIR_CLAUDE_API_KEY": "cl"}, "", "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearExplainEnv(t)
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
			if c.MemoirProvider != tc.wantProvider || c.MemoirModel != tc.wantModel || c.MemoirEffort != tc.wantEffort {
				t.Errorf("got %q %q %q, want %q %q %q", c.MemoirProvider, c.MemoirModel, c.MemoirEffort, tc.wantProvider, tc.wantModel, tc.wantEffort)
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
	clearExplainEnv(t)
	clearMemoirEnv(t)
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
	clearExplainEnv(t)
	clearMemoirEnv(t)
	t.Setenv("MEMOIR_CLAUDE_API_KEY", "cl")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MemoirRoute(); !strings.Contains(got, "Claude") || !strings.Contains(got, "claude-opus-5-5") || !strings.Contains(got, "medium") {
		t.Errorf("route %q", got)
	}
}
