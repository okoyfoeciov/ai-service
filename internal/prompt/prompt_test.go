package prompt

import (
	"strings"
	"testing"
)

// itemsList writes each phrase as a JSON string with & < > left as they are
// (like JavaScript's JSON.stringify); json.Marshal would send the model
// "R\u0026D" for the phrase the user chose.
func TestBuildItemsListNoHTMLEscape(t *testing.T) {
	p := Build("Our R&D <team> grew.", []Item{
		{Phrase: "R&D", Words: []string{"R&D"}},
		{Phrase: `<team> "lead"`, Words: []string{"team", "lead"}},
	}, nil, nil)
	want := part1 + `- "R&D"` + "\n" + `- "<team> \"lead\""` + part2
	if !strings.HasPrefix(p, want) {
		t.Fatalf("items list not JSON.stringify-shaped; prompt starts:\n%q", p[len(part1):min(len(p), len(part1)+80)])
	}
	if strings.Contains(p, `\u0026`) || strings.Contains(p, `\u003c`) || strings.Contains(p, `\u003e`) {
		t.Fatalf("items list is HTML-escaped")
	}
}
