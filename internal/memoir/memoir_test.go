package memoir

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf8"
)

// Prompt baseline, built with tag "K4FZZZX". The check pair pins the current
// check.txt, which follows the owner's language-check rule (see the package
// comment).
const (
	checkLen      = 6231
	checkSHA      = "933d006e59119dc70d8ad2d01bff9183ecffa67e399fa2f532c9c8af4b8772c8"
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
