// Package memoir holds the LLM contract for the Memoir check, served as
// /api/memoir/check.
//
// check.txt started from the owner's language-check rule in
// ~/.claude/CLAUDE.md (every mistake, correct-but-unnatural phrasing included,
// no cap on the count in the prompt) and since 2026-11-18 deliberately differs
// from it in three ways: spoken and emphatic constructions natives also write
// are not flagged; each passage is judged by its kind of writing (informal
// journal by default; work email, essay, chat message, fiction); and a
// non-native wording that reads as a possible choice of style is still
// reported, with a reason that starts "unusual, fine if deliberate:".
// TestPromptParity pins the rendered prompt byte for byte. A request may also
// carry the conversation the entry is the latest message of (comart's Ask
// card); conversation.txt is then inserted before the entry and the earlier
// turns are context only, never checked. The parser is
// deliberately lenient: it strips a code fence, salvages the {...} span, and
// never hands unparsed model text back as a correction.
//
// The clients are Server.memoirLLMs, separate from /api/explain's. This
// package is pure text — it never touches the network, so the prompt contract
// stays unit-testable without billing a turn.
package memoir

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf16"
)

//go:embed check.txt
var checkTmpl string

// conversationTmpl is inserted into checkTmpl just before entryHeader when the
// client sends the conversation the entry belongs to (comart's Ask card: the
// earlier questions and answers above the latest question). Without one the
// prompt is check.txt alone, byte for byte (TestPromptParity).
//
//go:embed conversation.txt
var conversationTmpl string

// entryHeader is the line of check.txt the conversation block goes in front of.
const entryHeader = "JOURNAL ENTRY — everything between"

// The size cap matches the clients' MEMOIR_CHECK_MAX. It is in UTF-16 code
// units — what JavaScript's String.length counts — and the server handler
// enforces it that way, not in bytes.
//
// CheckMaxIssues only stops a runaway reply; the prompt asks for every
// mistake. Keep it above what real writing produces: a 12000-character entry
// at one mistake every 30 characters is 400.
//
// ConversationMaxChars bounds the conversation sent with an entry, in the same
// UTF-16 units. FitConversation drops the oldest turns to stay under it.
const (
	CheckMaxChars        = 12000
	CheckMaxIssues       = 500
	ConversationMaxChars = 24000
)

// Turn is one earlier round of the conversation an entry belongs to: the
// author's message and the assistant's answer to it. Either may be empty (an
// answer that failed). Only text: no images and no model reasoning.
type Turn struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// UTF16Len counts s the way JavaScript's String.length does.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if l := utf16.RuneLen(r); l > 0 {
			n += l
		} else {
			n++
		}
	}
	return n
}

// truncateMark ends an answer FitConversation had to cut.
const truncateMark = " […]"

// turnLabels is what renderConversation adds around each turn's text
// ("AUTHOR:\n", "\n\nASSISTANT:\n" and the "\n\n" between turns), so the cap
// bounds the rendered conversation, not only its text.
const turnLabels = len("AUTHOR:\n") + len("\n\nASSISTANT:\n") + len("\n\n")

// FitConversation keeps the newest turns that fit in max UTF-16 units and
// reports how many older ones it dropped. Blank turns go first. When even the
// newest turn alone is too long, its answer is cut (from the end, ending in
// truncateMark) to fit; a question too long on its own drops the turn.
func FitConversation(turns []Turn, max int) (kept []Turn, dropped int) {
	var live []Turn
	for _, t := range turns {
		if strings.TrimSpace(t.Question) == "" && strings.TrimSpace(t.Answer) == "" {
			continue
		}
		live = append(live, t)
	}
	total := 0
	start := len(live)
	for i := len(live) - 1; i >= 0; i-- {
		n := UTF16Len(live[i].Question) + UTF16Len(live[i].Answer) + turnLabels
		if total+n > max {
			if i == len(live)-1 {
				room := max - UTF16Len(live[i].Question) - UTF16Len(truncateMark) - turnLabels
				if room > 0 {
					t := live[i]
					t.Answer = cutUTF16(t.Answer, room) + truncateMark
					return []Turn{t}, len(live) - 1
				}
				return nil, len(live)
			}
			break
		}
		total += n
		start = i
	}
	return append([]Turn(nil), live[start:]...), start
}

// cutUTF16 returns the longest prefix of s that is at most max UTF-16 units,
// never splitting a rune.
func cutUTF16(s string, max int) string {
	n := 0
	for i, r := range s {
		l := utf16.RuneLen(r)
		if l <= 0 {
			l = 1
		}
		if n+l > max {
			return strings.TrimRight(s[:i], " \t\n")
		}
		n += l
	}
	return s
}

// renderConversation writes turns the way conversationTmpl describes them.
func renderConversation(turns []Turn) string {
	var b strings.Builder
	for i, t := range turns {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("AUTHOR:\n")
		b.WriteString(t.Question)
		b.WriteString("\n\nASSISTANT:\n")
		b.WriteString(t.Answer)
	}
	return b.String()
}

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

// buildWithTag renders checkTmpl with an explicit tag (the parity test uses it;
// production passes newTag).
func buildWithTag(content, tag string) string {
	s := strings.ReplaceAll(checkTmpl, "__TAG__", tag)
	return strings.ReplaceAll(s, "__CONTENT__", content)
}

// buildConversationWithTag renders checkTmpl with the conversation block in
// front of the entry. One Replacer pass fills every placeholder, so text in
// the conversation or the entry that happens to spell a placeholder is never
// expanded.
//
// The conversation has its own fence tag (ctag), so the only markers carrying
// the entry's tag are the entry's own two: check.txt's "everything between
// the two <tag> markers" stays exact.
func buildConversationWithTag(content string, turns []Turn, tag, ctag string) string {
	at := strings.Index(checkTmpl, entryHeader)
	if at < 0 {
		panic("memoir: check.txt lost its entry header")
	}
	tmpl := checkTmpl[:at] + conversationTmpl + checkTmpl[at:]
	return strings.NewReplacer(
		"__CTAG__", ctag,
		"__TAG__", tag,
		"__CONTENT__", content,
		"__CONVERSATION__", renderConversation(turns),
	).Replace(tmpl)
}

// BuildCheckPrompt renders the proofreading prompt for a whole journal entry.
func BuildCheckPrompt(content string) string {
	return buildWithTag(content, newTag(content))
}

// BuildCheckPromptInConversation renders the prompt for an entry that is the
// latest message of a conversation: the earlier turns are context only, and
// only the entry is checked. With no turns it is BuildCheckPrompt.
func BuildCheckPromptInConversation(content string, turns []Turn) string {
	if len(turns) == 0 {
		return BuildCheckPrompt(content)
	}
	conv := renderConversation(turns)
	tag := newTag(content + "\n" + conv)
	ctag := newTag(content + "\n" + conv + "\n" + tag)
	return buildConversationWithTag(content, turns, tag, ctag)
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

// CheckResult is a parsed model reply: the corrected entry and its issues.
type CheckResult struct {
	Corrected string
	Issues    []CheckIssue
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
func TryParseCheckResult(raw string) *CheckResult {
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

func parseCheck(s string) *CheckResult {
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
	return &CheckResult{Corrected: j.Corrected, Issues: j.Issues}
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
// the author's own. It catches a placeholder ("<same as input>"), a reply cut
// off at the token limit and a short unrelated blob: the text must not be a
// placeholder and must be at least 60% of the entry's length.
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
