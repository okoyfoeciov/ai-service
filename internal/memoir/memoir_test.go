package memoir

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf8"
)

// Prompt baseline, built with tag "K4FZZZX". The check pair pins the current
// check.txt (see the package comment for how it differs from the owner's
// language-check rule).
const (
	checkLen      = 10431
	checkSHA      = "200475606bdd54d4696fdb8f30e2e3d6e420a16c3d519f38e1cd85a02b134478"
	parityTag     = "K4FZZZX"
	parityCheckIn = "Yesterday I go to office at morning.\n\nRain. Coffee."
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestPromptParity pins the prompt byte for byte against the baseline above.
// Lengths are in runes; the SHA-256 is over bytes.
func TestPromptParity(t *testing.T) {
	got := buildWithTag(parityCheckIn, parityTag)
	if utf8.RuneCountInString(got) != checkLen || sha(got) != checkSHA {
		t.Fatalf("check prompt drift: runes=%d sha=%s (want %d %s)",
			utf8.RuneCountInString(got), sha(got), checkLen, checkSHA)
	}
}

// TestTagShape: every request gets a seven-character K-prefixed marker that
// does not occur in the content. Collision avoidance is best-effort here; the
// loop in newTag is what guarantees it.
func TestTagShape(t *testing.T) {
	for i := 0; i < 50; i++ {
		content := "entry K4FZZZX entry"
		tag := newTag(content)
		if strings.Contains(content, tag) {
			t.Fatalf("tag %q collides with content", tag)
		}
		if len(tag) != 7 || tag[0] != 'K' {
			t.Fatalf("tag %q has wrong shape", tag)
		}
	}
}

func TestParseCheck(t *testing.T) {
	raw := "{\"corrected\":\"A B.\",\"issues\":[{\"category\":\"tense\",\"original\":\"go\",\"suggestion\":\"went\",\"reason\":\"past tense\"}]}"
	out := TryParseCheckResult(raw)
	if out == nil || out.Corrected != "A B." || len(out.Issues) != 1 {
		t.Fatalf("direct parse failed: %+v", out)
	}
	// Fenced.
	out = TryParseCheckResult("```json\n" + raw + "\n```")
	if out == nil || out.Corrected != "A B." {
		t.Fatalf("fenced parse failed")
	}
	// Prose around the object.
	out = TryParseCheckResult("here:\n" + raw + "\nthanks")
	if out == nil || out.Corrected != "A B." {
		t.Fatalf("salvage parse failed")
	}
	// No corrected key: unusable, never served as a correction.
	if TryParseCheckResult("{\"variants\":[]}") != nil {
		t.Fatalf("check-shaped gate accepted an unrelated reply")
	}
	if TryParseCheckResult("not json at all") != nil {
		t.Fatalf("garbage accepted")
	}
}

func TestSanityFloor(t *testing.T) {
	if SanityFloor("<same as input>", "a long entry here") {
		t.Fatalf("placeholder accepted")
	}
	if SanityFloor("short", strings.Repeat("x", 100)) {
		t.Fatalf("truncated correction accepted")
	}
	if !SanityFloor("a fine correction of similar length", "a fine entry of similar size!!") {
		t.Fatalf("good correction refused")
	}
}

// TestCheckPromptFollowsLanguageRule: check.txt follows the owner's language
// check rule (~/.claude/CLAUDE.md): every mistake, correct-but-unnatural
// phrasing included, no cap on the count and no register exemption.
func TestCheckPromptFollowsLanguageRule(t *testing.T) {
	for _, want := range []string{"still flag it", "however many there are"} {
		if !strings.Contains(checkTmpl, want) {
			t.Errorf("check prompt lacks %q", want)
		}
	}
	for _, bad := range []string{"at most", "Minimal means", "do not pad", "Note-style shorthand", "unfamiliar capitalised"} {
		if strings.Contains(checkTmpl, bad) {
			t.Errorf("check prompt still holds back: %q", bad)
		}
	}
}

// TestConversationPrompt: with earlier turns the conversation block sits just
// before the entry, the turns are labelled, and nothing else of check.txt
// moves. Without turns the prompt is the plain check, byte for byte.
func TestConversationPrompt(t *testing.T) {
	turns := []Turn{
		{Question: "What does charge mean here?", Answer: "It means to accuse formally."},
		{Question: "And the second one?", Answer: "To ask a price."},
	}
	got := buildConversationWithTag(parityCheckIn, turns, parityTag, "KCONVTG")
	plain := buildWithTag(parityCheckIn, parityTag)
	at := strings.Index(got, "CONVERSATION\nFor this check")
	if at < 0 {
		t.Fatal("conversation block missing")
	}
	end := strings.Index(got, "<<<END_CONVERSATION_KCONVTG>>>\n\n")
	if end < 0 {
		t.Fatal("conversation fence missing")
	}
	block := got[at : end+len("<<<END_CONVERSATION_KCONVTG>>>\n\n")]
	if strings.Replace(got, block, "", 1) != plain {
		t.Fatal("the conversation block changed check.txt outside itself")
	}
	if !strings.HasPrefix(got[at+len(block):], entryHeader) {
		t.Fatal("conversation block is not right before the entry header")
	}
	if strings.Count(got, parityTag) != strings.Count(plain, parityTag) {
		t.Fatal("the entry's tag must mark only the entry")
	}
	want := "<<<CONVERSATION_KCONVTG>>>\nAUTHOR:\nWhat does charge mean here?\n\nASSISTANT:\nIt means to accuse formally.\n\n" +
		"AUTHOR:\nAnd the second one?\n\nASSISTANT:\nTo ask a price.\n<<<END_CONVERSATION_KCONVTG>>>"
	if !strings.Contains(block, want) {
		t.Fatalf("turns rendered wrong:\n%s", block)
	}
	if BuildCheckPromptInConversation(parityCheckIn, nil) == "" ||
		strings.Contains(BuildCheckPromptInConversation(parityCheckIn, nil), "CONVERSATION") {
		t.Fatal("no turns must give the plain prompt")
	}
}

// TestConversationPlaceholdersInert: a turn or entry that spells a template
// placeholder is copied through, never expanded.
func TestConversationPlaceholdersInert(t *testing.T) {
	turns := []Turn{{Question: "what is __CONTENT__?", Answer: "and __TAG__ and __CTAG__ and __CONVERSATION__"}}
	got := buildConversationWithTag("my __CONVERSATION__ entry", turns, parityTag, "KCONVTG")
	for _, s := range []string{"what is __CONTENT__?", "and __TAG__ and __CTAG__ and __CONVERSATION__", "my __CONVERSATION__ entry"} {
		if !strings.Contains(got, s) {
			t.Fatalf("%q was expanded", s)
		}
	}
	if strings.Count(got, "my __CONVERSATION__ entry") != 1 {
		t.Fatal("entry appears more than once")
	}
}

// TestConversationTagAvoidsTurns: the fence tag never occurs in the turns.
func TestConversationTagAvoidsTurns(t *testing.T) {
	turns := []Turn{{Question: "q", Answer: "a"}}
	conv := renderConversation(turns)
	for i := 0; i < 50; i++ {
		tag := newTag("entry\n" + conv)
		if strings.Contains(conv, tag) {
			t.Fatal("tag collides")
		}
	}
}

func TestFitConversation(t *testing.T) {
	ten := strings.Repeat("x", 10)
	turns := []Turn{
		{Question: ten, Answer: ten}, // 20
		{Question: " ", Answer: ""},  // blank: skipped
		{Question: ten, Answer: ten}, // 20
		{Question: ten, Answer: ten}, // 20
	}
	kept, dropped := FitConversation(turns, 2*(20+turnLabels)+5)
	if len(kept) != 2 || dropped != 1 {
		t.Fatalf("kept %d dropped %d, want 2 and 1", len(kept), dropped)
	}
	kept, dropped = FitConversation(turns, 3*(20+turnLabels))
	if len(kept) != 3 || dropped != 0 {
		t.Fatalf("kept %d dropped %d, want 3 and 0", len(kept), dropped)
	}
	// The newest turn alone is too long: its answer is cut, the question kept.
	long := []Turn{{Question: "old", Answer: "old"}, {Question: "q?", Answer: strings.Repeat("é", 100)}}
	kept, dropped = FitConversation(long, 30+turnLabels)
	if len(kept) != 1 || dropped != 1 || kept[0].Question != "q?" ||
		!strings.HasSuffix(kept[0].Answer, truncateMark) ||
		UTF16Len(renderConversation(kept)) > 30+turnLabels || !utf8.ValidString(kept[0].Answer) {
		t.Fatalf("cut wrong: %+v dropped %d", kept, dropped)
	}
	// A question longer than the budget drops everything.
	kept, dropped = FitConversation([]Turn{{Question: strings.Repeat("q", 50), Answer: "a"}}, 30)
	if len(kept) != 0 || dropped != 1 {
		t.Fatalf("kept %d dropped %d, want 0 and 1", len(kept), dropped)
	}
	// The cap bounds the rendered conversation, labels included.
	many := make([]Turn, 500)
	for i := range many {
		many[i] = Turn{Question: "q", Answer: "a"}
	}
	kept, _ = FitConversation(many, 1000)
	if UTF16Len(renderConversation(kept)) > 1000 {
		t.Fatalf("rendered %d > 1000", UTF16Len(renderConversation(kept)))
	}
	// Surrogate pairs count 2.
	if UTF16Len("a😀") != 3 {
		t.Fatal("UTF16Len")
	}
}
