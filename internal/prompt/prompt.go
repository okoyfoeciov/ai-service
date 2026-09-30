// Package prompt assembles the vocabulary-coach prompt for /api/explain. The
// large static skeleton (including the Merriam-Webster pronunciation key) lives
// in part1.txt/part2.txt; only the dynamic pieces are assembled here.
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"llm-service/internal/mw"
)

//go:embed part1.txt
var part1 string

//go:embed part2.txt
var part2 string

// Item is one chosen word or phrase from the explain request.
type Item struct {
	Phrase string   `json:"phrase"`
	Words  []string `json:"words"`
}

// Build lays the prompt out as:
//
//	part1 + itemsList + part2 + text + "\n" + earlierSection + pronSection
func Build(text string, items []Item, priorReplies []string, pronBundle map[string]mw.Bundle) string {
	return part1 + itemsList(items) + part2 + text + "\n" + earlierSection(priorReplies) + pronSection(items, pronBundle)
}

func itemsList(items []Item) string {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		// Each phrase is a JSON string without json.Marshal's HTML escaping, so
		// the model sees "R&D", not "R\u0026D". Encode appends a newline; drop it.
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(it.Phrase)
		lines = append(lines, "- "+strings.TrimSuffix(b.String(), "\n"))
	}
	return strings.Join(lines, "\n")
}

func earlierSection(priorReplies []string) string {
	if len(priorReplies) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# EARLIER EXPLANATIONS\n\nThese are your previous replies to this same user, ordered oldest first. They are still visible to the user above your new reply. The user may have chosen items from these — explain those items in the sense the words were used HERE, and do NOT repeat any explanation already given.\n\n")
	parts := make([]string, 0, len(priorReplies))
	for i, r := range priorReplies {
		parts = append(parts, fmt.Sprintf("## Earlier reply %d\n\n%s", i+1, r))
	}
	b.WriteString(strings.Join(parts, "\n\n"))
	b.WriteString("\n")
	return b.String()
}

func pronSection(items []Item, pronBundle map[string]mw.Bundle) string {
	var pronLines []string
	for _, it := range items {
		if len(it.Words) != 1 {
			continue
		}
		w := strings.ToLower(it.Words[0])
		bundle, ok := pronBundle[w]
		if !ok || len(bundle.Variants) == 0 {
			continue
		}
		var vlines []string
		for _, v := range bundle.Variants {
			posStr := `"(any)"`
			if len(v.PosList) > 0 {
				quoted := make([]string, len(v.PosList))
				for i, p := range v.PosList {
					quoted[i] = `"` + p + `"`
				}
				posStr = strings.Join(quoted, " or ")
			}
			vlines = append(vlines, "    • pos="+posStr+" → /"+v.IPA+"/")
		}
		pronLines = append(pronLines, "  - "+w+":\n"+strings.Join(vlines, "\n"))
	}
	if len(pronLines) == 0 {
		return ""
	}
	prefix := "\n# PRONUNCIATIONS\n\nMerriam-Webster lists these distinct pronunciations for the chosen single words. When you emit [[PRON word=\"X\" pos=\"Y\" ipa=\"…\"]], pos MUST be one of the strings shown for that word VERBATIM (including any \"(1)\" / \"(2)\" suffix). Pick the one that matches the sense you're explaining. These words HAVE a real dictionary pronunciation, so the app shows MW's audio badge and ignores your ipa for them — but still fill ipa in (it costs nothing and keeps the rule uniform). Match MW's own respelling style, set out in the MERRIAM-WEBSTER PRONUNCIATION NOTATION key.\n\n"
	return prefix + strings.Join(pronLines, "\n") + "\n"
}
