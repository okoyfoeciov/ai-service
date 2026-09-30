package memoir

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf8"
)

// Prompt baselines, built with tag "K4FZZZX". The paraphrase pair was taken by
// running the original Node server's builder with Math.random stubbed to
// 0.123456789; the Node source no longer exists, so it is the reference now.
// The check pair pins the current check.txt, which follows the owner's
// language-check rule, not the Node original (see the package comment).
const (
	checkLen      = 6231
	checkSHA      = "933d006e59119dc70d8ad2d01bff9183ecffa67e399fa2f532c9c8af4b8772c8"
	nodeParaLen   = 12757
	nodeParaSHA   = "dda2bd603b9c4ee84f7c85a16a8d71ae4cb11ccea59a78e2c9c8f16ed7a81a83"
	parityTag     = "K4FZZZX"
	parityCheckIn = "Yesterday I go to office at morning.\n\nRain. Coffee."
	parityParaIn  = "very tired after the long meeting"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestPromptParity pins both prompts byte for byte against the baselines above.
// Lengths are in runes, which match the Node paraphrase baseline's UTF-16 count
// for these prompts (an em dash is 1 in both, 3 in Go's len); the SHA-256 is
// over bytes.
func TestPromptParity(t *testing.T) {
	got := buildWithTag(checkTmpl, parityCheckIn, parityTag)
	if utf8.RuneCountInString(got) != checkLen || sha(got) != checkSHA {
		t.Fatalf("check prompt drift: runes=%d sha=%s (want %d %s)",
			utf8.RuneCountInString(got), sha(got), checkLen, checkSHA)
	}
	got = buildWithTag(paraphraseTmpl, parityParaIn, parityTag)
	if utf8.RuneCountInString(got) != nodeParaLen || sha(got) != nodeParaSHA {
		t.Fatalf("paraphrase prompt drift: runes=%d sha=%s (want %d %s)",
			utf8.RuneCountInString(got), sha(got), nodeParaLen, nodeParaSHA)
	}
}

// TestTagAvoidsContent: a tag that occurs in the content must never be used.
func TestTagAvoidsContent(t *testing.T) {
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
		t.Fatalf("check-shaped gate accepted a paraphrase reply")
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

func TestParseParaphrase(t *testing.T) {
	raw := "{\"variants\":[{\"label\":\"natural\",\"text\":\"Worn out after the meeting\",\"focus\":\"Worn out\",\"note\":\"'worn out' means very tired\"}]}"
	v, ok := TryParseParaphraseResult(raw)
	if !ok || len(v) != 1 || v[0].Label != "natural" {
		t.Fatalf("direct parse failed: %+v %v", v, ok)
	}
	// Deliberate empty stays ok=true with no variants (legitimate answer).
	v, ok = TryParseParaphraseResult("{\"variants\":[]}")
	if !ok || len(v) != 0 {
		t.Fatalf("empty variants mishandled")
	}
	// Bare array.
	v, ok = TryParseParaphraseResult("[{\"label\":\"idiom\",\"text\":\"Running on empty\"}]")
	if !ok || len(v) != 1 {
		t.Fatalf("bare array failed")
	}
	// Error envelope must not read as "no alternatives".
	if _, ok = TryParseParaphraseResult("{\"error\":{\"message\":\"boom\"}}"); ok {
		t.Fatalf("error envelope accepted")
	}
	if _, ok = TryParseParaphraseResult("nothing usable"); ok {
		t.Fatalf("garbage accepted")
	}
}

func TestNormalizeVariants(t *testing.T) {
	src := "very tired after the long meeting"
	in := []Variant{
		{Label: "natural", Text: "very tired after the long meeting", Note: "echo"}, // the span itself
		{Label: "natural", Text: "WORN OUT after the long meeting", Focus: "WORN OUT", Note: "x"},
		{Label: "weird label", Text: "Drained by the long meeting", Note: "y"},
		{Label: "simpler", Text: "Drained by the long meeting", Note: "dup"}, // repeat
		{Label: "simpler", Text: "Tired", Note: "gutted"},
		{Label: "idiom", Text: "Running on empty after the long meeting", Focus: "absent phrase", Note: "z"},
	}
	out := NormalizeVariants(in, src)
	if len(out) != 3 {
		t.Fatalf("want 3 survivors, got %d: %+v", len(out), out)
	}
	// Canonical order: natural, natural ("weird label" defaults to natural), idiom.
	if out[0].Label != "natural" || out[1].Label != "natural" || out[2].Label != "idiom" {
		t.Fatalf("bad labels/order: %+v", out)
	}
	if out[2].Focus != "" {
		t.Fatalf("focus not in text kept: %+v", out[2])
	}
	if out[0].Focus != "WORN OUT" {
		t.Fatalf("exact-case focus dropped: %+v", out[0])
	}
}

// A note over 160 characters is cut by character, never mid-rune: the em dash
// straddling byte 160 must survive whole or not at all.
func TestNormalizeVariantsNoteTruncatesByRune(t *testing.T) {
	note := strings.Repeat("a", 159) + "—" + strings.Repeat("b", 20) // "—" is bytes 159-161
	out := NormalizeVariants([]Variant{{Label: "natural", Text: "Worn out after the long meeting", Note: note}},
		"very tired after the long meeting")
	if len(out) != 1 {
		t.Fatalf("want 1 survivor, got %d: %+v", len(out), out)
	}
	got := out[0].Note
	if !utf8.ValidString(got) {
		t.Fatalf("note split a character: %q", got)
	}
	if want := strings.Repeat("a", 159) + "—"; got != want {
		t.Fatalf("note = %q, want the first 160 characters %q", got, want)
	}
}

// A wrapping quote pair or list marker is the model's dressing only when the
// author's own span lacks it. When the span is quoted speech or a list item the
// wrapping is the author's: stripping it would unbalance the quotes or drop the
// marker the author wrote.
func TestNormalizeVariantsKeepsAuthorsWrapping(t *testing.T) {
	const plain = "very tired after the long meeting"
	cases := []struct{ name, src, text, want string }{
		{"model's quotes stripped", plain,
			`"Worn out after the long meeting"`, "Worn out after the long meeting"},
		{"model's curly quotes stripped", plain,
			"“Worn out after the long meeting”", "Worn out after the long meeting"},
		{"quoted speech keeps its quotes", `"I am so tired," she said, "after that meeting."`,
			`"I'm exhausted," she said, "after that meeting."`, `"I'm exhausted," she said, "after that meeting."`},
		{"curly quoted speech keeps its quotes", "“I am so tired,” she said, “after that meeting.”",
			"“I'm worn out,” she said, “after that meeting.”", "“I'm worn out,” she said, “after that meeting.”"},
		{"single-quoted span keeps its quotes", "'very tired after the long meeting'",
			"'worn out after the long meeting'", "'worn out after the long meeting'"},
		{"model's list marker stripped", plain,
			"1. Worn out after the long meeting", "Worn out after the long meeting"},
		{"author's list item keeps its marker", "- very tired after the long meeting",
			"- worn out after the long meeting", "- worn out after the long meeting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := NormalizeVariants([]Variant{{Label: "natural", Text: tc.text}}, tc.src)
			if len(out) != 1 {
				t.Fatalf("want 1 survivor, got %d: %+v", len(out), out)
			}
			if out[0].Text != tc.want {
				t.Fatalf("text = %q, want %q", out[0].Text, tc.want)
			}
		})
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
