// Package memoir holds the LLM contract for Cairn's Memoir tab: proofread an
// entry (check) and other ways to say a selected span (paraphrase), served as
// /api/memoir/check and /api/memoir/paraphrase.
//
// paraphrase.txt comes from Memoir's original Node server. check.txt follows
// the owner's language-check rule in ~/.claude/CLAUDE.md instead: every
// mistake, correct-but-unnatural phrasing included, no cap on the count in the
// prompt and no register exemption; keep check.txt in step with that rule.
// TestPromptParity pins both prompts
// byte for byte (paraphrase against a hash taken from the Node builder, which
// no longer exists). The parsing below follows the Node server's rules:
// strip fences, salvage the {...} span (or [...] for paraphrase), never hand
// unparsed model text back as a correction, drop echo, duplicate and truncated
// variants.
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
	"sort"
	"strings"
	"unicode/utf8"
)

//go:embed check.txt
var checkTmpl string

//go:embed paraphrase.txt
var paraphraseTmpl string

// The two size caps match Cairn's MEMOIR_CHECK_MAX and MEMOIR_PARA_MAX. They
// are in UTF-16 code units — what JavaScript's String.length counts — and the
// server handlers enforce them that way, not in bytes. A paraphrase is one
// selected span, never the whole entry: the reply holds up to four rewrites of
// it, and a reply cut off at the answer budget loses every alternative, not one.
//
// CheckMaxIssues only stops a runaway reply; the prompt asks for every
// mistake. Keep it above what real writing produces: a 12000-character entry
// at one mistake every 30 characters is 400.
const (
	CheckMaxChars      = 12000
	ParaphraseMaxChars = 600
	CheckMaxIssues     = 500
)

// ParaLabels is the canonical label order — slot position means the same thing
// on every run, so results are sorted into this order before returning.
var ParaLabels = []string{"natural", "simpler", "restructured", "idiom", "formal"}

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

// BuildParaphrasePrompt renders the other-ways prompt for one selected span.
func BuildParaphrasePrompt(content string) string {
	return buildWithTag(paraphraseTmpl, content, newTag(content))
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
	// is not a check result (a paraphrase-shaped reply, an error envelope).
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

// ---------------------------------------------------------------------------
// Paraphrase results
// ---------------------------------------------------------------------------

// Variant is one alternative phrasing.
type Variant struct {
	Label string `json:"label"`
	Text  string `json:"text"`
	Focus string `json:"focus"`
	Note  string `json:"note"`
}

// TryParseParaphraseResult recovers the variant list from raw model text. An
// unparseable reply is a failure (the handler answers 502) while a deliberate
// {"variants":[]} is a legitimate "no alternatives", so the two stay distinct:
// ok=false for the first, an empty list with ok=true for the second.
func TryParseParaphraseResult(raw string) ([]Variant, bool) {
	text := stripFences(raw)
	if out, ok := takeVariants(text, false); ok {
		return out, true
	}
	if fb, lb := strings.Index(text, "{"), strings.LastIndex(text, "}"); fb != -1 && lb > fb {
		if out, ok := takeVariants(text[fb:lb+1], false); ok {
			return out, true
		}
	}
	if fa, la := strings.Index(text, "["), strings.LastIndex(text, "]"); fa != -1 && la > fa {
		if out, ok := takeVariants(text[fa:la+1], true); ok {
			return out, true
		}
	}
	return nil, false
}

// takeVariants accepts a bare array, an object with a variants array (or a
// single variants object), or a single {text,...} object. needItems gates the
// salvaged bracket slice: it has to actually carry variants, or an error
// envelope yields [] and gets reported as a legitimate "no alternatives"
// instead of a failure.
func takeVariants(s string, needItems bool) ([]Variant, bool) {
	var j any
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		return nil, false
	}
	var arr []any
	switch v := j.(type) {
	case []any:
		arr = v
	case map[string]any:
		if vv, ok := v["variants"]; ok {
			if a, ok := vv.([]any); ok {
				arr = a
			} else if o, ok := vv.(map[string]any); ok {
				arr = []any{o}
			} else {
				return nil, false
			}
		} else if t, ok := v["text"].(string); ok && t != "" {
			arr = []any{v}
		} else {
			return nil, false
		}
	default:
		return nil, false
	}
	if needItems {
		found := false
		for _, item := range arr {
			if o, ok := item.(map[string]any); ok {
				if t, ok := o["text"].(string); ok && t != "" {
					found = true
					break
				}
			}
		}
		if !found {
			return nil, false
		}
	}
	out := make([]Variant, 0, len(arr))
	for _, item := range arr {
		o, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var v Variant
		v.Label, _ = o["label"].(string)
		v.Text, _ = o["text"].(string)
		v.Focus, _ = o["focus"].(string)
		v.Note, _ = o["note"].(string)
		out = append(out, v)
	}
	return out, true
}

var (
	wsRe      = regexp.MustCompile(`\s+`)
	edgePunct = regexp.MustCompile(`^[^\p{L}\p{N}]+|[^\p{L}\p{N}]+$`)
	listRe    = regexp.MustCompile(`^(?:[-*•]\s+|\d+[.)]\s+|variant\s*\d*\s*[:.\-]\s*)`)
)

// normKey lower-cases, collapses whitespace and strips edge punctuation. Used
// for both "is this just the span again?" and "did two slots come back the same
// sentence in different clothes?".
func normKey(s string) string {
	t := strings.ToLower(wsRe.ReplaceAllString(s, " "))
	t = strings.TrimSpace(t)
	return edgePunct.ReplaceAllString(t, "")
}

// NormalizeVariants is the per-variant analogue of the sanity floor on
// `corrected`. Everything that fails is dropped silently; returning [] is a
// legitimate answer, so an empty result is never in itself an error.
func NormalizeVariants(list []Variant, source string) []Variant {
	srcKey := normKey(source)
	src := strings.TrimSpace(source)
	srcIsListItem := listRe.MatchString(src)
	seen := map[string]bool{}
	out := []Variant{}
	for _, item := range list {
		text := strings.TrimSpace(item.Text)
		// The model sometimes dresses the variant as a list entry despite the rule
		// — unless the author's own span is one, when the marker is theirs.
		if !srcIsListItem {
			text = strings.TrimSpace(listRe.ReplaceAllString(text, ""))
		}
		// Likewise a wrapping quote pair, unless the span itself is wrapped in the
		// same pair (quoted speech): stripping it there unbalances the quotes.
		for _, q := range [][2]string{{`"`, `"`}, {"“", "”"}, {`'`, `'`}, {"‘", "’"}} {
			if wrapped(text, q) {
				if !wrapped(src, q) {
					text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, q[0]), q[1]))
				}
				break
			}
		}
		if text == "" {
			continue
		}
		if placeholderRe.MatchString(text) {
			continue // "<same idea>" placeholder
		}
		if len(text)*10 < len(source)*4 {
			continue // gutted, or truncated by max_completion_tokens
		}
		if len(text) > len(source)*25/10 {
			continue // wandered off into commentary
		}
		key := normKey(text)
		if key == "" || key == srcKey || seen[key] {
			continue // the span itself, or a repeat
		}
		seen[key] = true
		label := strings.ToLower(strings.TrimSpace(wsRe.ReplaceAllString(item.Label, "_")))
		if !paraLabelOK(label) {
			label = "natural"
		}
		note := item.Note
		if len(note) > 0 {
			note = wsRe.ReplaceAllString(strings.TrimSpace(note), " ")
			if utf8.RuneCountInString(note) > 160 {
				note = string([]rune(note)[:160]) // never split a character
			}
		}
		focus := strings.TrimSpace(item.Focus)
		if focus == "" || !strings.Contains(text, focus) {
			focus = "" // cannot underline what is not there
		}
		out = append(out, Variant{Label: label, Text: text, Focus: focus, Note: note})
	}
	// Canonical order, so slot position means the same thing on every run.
	rank := map[string]int{}
	for i, l := range ParaLabels {
		rank[l] = i
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Label] < rank[out[j].Label] })
	if len(out) > 6 {
		out = out[:6] // trust the prompt for quality, never for bounds
	}
	return out
}

// wrapped reports whether s starts with q[0] and ends with q[1] as two separate
// marks (a lone `"` is not a wrapping pair).
func wrapped(s string, q [2]string) bool {
	return len(s) > 1 && strings.HasPrefix(s, q[0]) && strings.HasSuffix(s, q[1])
}

func paraLabelOK(l string) bool {
	for _, p := range ParaLabels {
		if p == l {
			return true
		}
	}
	return false
}
