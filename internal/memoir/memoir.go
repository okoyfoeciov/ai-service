// Package memoir holds the LLM contract for Cairn's Memoir tab: proofread an
// entry (check), served as /api/memoir/check.
//
// check.txt follows the owner's language-check rule in ~/.claude/CLAUDE.md
// instead: every mistake, correct-but-unnatural phrasing included, no cap on
// the count in the prompt and no register exemption; keep check.txt in step
// with that rule. TestPromptParity pins the prompt byte for byte. The parsing
// below follows the Node server's rules: strip fences, salvage the {...} span,
// never hand unparsed model text back as a correction, drop echo, duplicate and
// truncated issues.
//
// The model is Server.memoirLLM, separate from the one /api/explain uses
// (MEMOIR_MODEL, default Claude Opus 5.5; Qwen 3.8 27B on Cerebras if that key
// is set instead). This package is pure text — it never touches the network, so
// the prompt contract stays unit-testable without billing a turn.
package memoir

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
)

//go:embed check.txt
var checkTmpl string

// The size cap matches Cairn's MEMOIR_CHECK_MAX. It is in UTF-16 code units —
// what JavaScript's String.length counts — and the server handler enforces it
// that way, not in bytes.
//
// CheckMaxIssues only stops a runaway reply; the prompt asks for every
// mistake. Keep it above what real writing produces: a 12000-character entry
// at one mistake every 30 characters is 400.
const (
	CheckMaxChars  = 12000
	CheckMaxIssues = 500
)

// newTag makes one per-request fence marker. Never sanitise the content to avoid
// a collision: altering the text the model proofreads would break verbatim
// `original` spans against the editor. Draw a new tag instead.
func newTag(content string) string {
	const syms = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for {
		var b [7]byte
		b[0] = 'K'
		var rb [6]byte
		rand.Read(rb[:]) // never returns an error (it crashes the program instead)
		for i, v := range rb {
			b[i+1] = syms[int(v)%36]
		}
		tag := string(b[:])
		if !strings.Contains(content, tag) {
			return tag
		}
	}
}

// buildWithTag renders tmpl with an explicit tag (tests use it for parity;
// production passes newTag).
func buildWithTag(tmpl, content, tag string) string {
	s := strings.ReplaceAll(tmpl, "__TAG__", tag)
	return strings.ReplaceAll(s, "__CONTENT__", content)
}

// BuildCheckPrompt renders the proofreading prompt for a whole journal entry.
func BuildCheckPrompt(content string) string {
	return buildWithTag(checkTmpl, content, newTag(content))
}

// ---------------------------------------------------------------------------
// Check results
// ---------------------------------------------------------------------------

// CheckIssue is one fix from the model.
type CheckIssue struct {
	Category   string `json:"category"`
	Original   string `json:"original"`
	Suggestion string `json:"suggestion"`
	Reason     string `json:"reason"`
}

var fenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// stripFences removes a wrapping code fence, as the model sometimes adds one
// despite the no-fence instruction.
func stripFences(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	if m := fenceRe.FindStringSubmatch(t); m != nil {
		return strings.TrimSpace(m[1])
	}
	return t
}

// TryParseCheckResult recovers {corrected, issues} from raw model text, or
// returns nil when there is nothing usable. On nil the caller serves the
// author's own text as `corrected`, never the raw model text: clients show
// `corrected` as the checked entry.
func TryParseCheckResult(raw string) *struct {
	Corrected string
	Issues    []CheckIssue
} {
	text := stripFences(raw)
	if out := parseCheck(text); out != nil {
		return out
	}
	fb, lb := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if fb != -1 && lb > fb {
		if out := parseCheck(text[fb : lb+1]); out != nil {
			return out
		}
	}
	return nil
}

func parseCheck(s string) *struct {
	Corrected string
	Issues    []CheckIssue
} {
	var j struct {
		Corrected string       `json:"corrected"`
		Issues    []CheckIssue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		return nil
	}
	// json.Unmarshal already fails on a non-string `corrected`, but it takes a
	// missing or null one as "". Refuse those too: without `corrected` the reply
	// is not a check result (an unrelated shape, an error envelope).
	if !jsonHasString(s, "corrected") {
		return nil
	}
	if j.Issues == nil {
		j.Issues = []CheckIssue{}
	}
	return &struct {
		Corrected string
		Issues    []CheckIssue
	}{Corrected: j.Corrected, Issues: j.Issues}
}

// jsonHasString reports whether the top-level object carries a string value
// under key (present-but-null and wrong-type both fail).
func jsonHasString(s, key string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	_, ok = v.(string)
	return ok
}

// placeholderRe matches a reply that is only a placeholder ("<same as input>",
// "<same idea>") instead of text.
var placeholderRe = regexp.MustCompile(`^<[^>]*>$`)

// SanityFloor reports whether the model's corrected text is plausible next to
// the author's own. One check catches a placeholder ("<same as input>"), a
// reply cut off by max_completion_tokens and a short unrelated blob: the text must not be
// a placeholder and must be at least 60% of the entry's length.
func SanityFloor(corrected, content string) bool {
	t := strings.TrimSpace(corrected)
	if placeholderRe.MatchString(t) {
		return false
	}
	if content != "" && len(corrected) < len(content)*6/10 {
		return false
	}
	return true
}
