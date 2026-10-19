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

func TestLoadRefusesBothExplainKeys(t *testing.T) {
	clearExplainEnv(t)
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
				if strings.Contains(m, "CLAUDE_API_KEY") {
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
	t.Setenv("OPENROUTER_API_KEY", "or")
	t.Setenv("CLAUDE_API_KEY", "")
	c, _, err := Load()
	if err != nil || c.ExplainProvider != ProviderOpenRouter {
		t.Fatalf("got %q, %v", c.ExplainProvider, err)
	}
}

func TestExplainRouteNamesTheProviderAndModel(t *testing.T) {
	clearExplainEnv(t)
	t.Setenv("CLAUDE_API_KEY", "cl")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ExplainRoute(); !strings.Contains(got, "Claude") || !strings.Contains(got, "claude-opus-5-5") || !strings.Contains(got, "medium") {
		t.Errorf("route %q", got)
	}
}
