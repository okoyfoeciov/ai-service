package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf16"

	"llm-service/internal/llm"
	"llm-service/internal/memoir"
)

// Memoir handlers for Cairn's Memoir tab: proofread-an-entry (check) and
// other-ways-to-say-it (paraphrase). The limits and response shapes are what
// Cairn's src/memoir.ts expects; keep them stable. Unparsed model text is
// never handed back as a correction.
//
// Unlike /api/explain these are non-streaming JSON: the model must finish
// before any alternative is usable (one truncated variant list loses every
// alternative, not one), so the handler collects the stream and responds once.
// The model is Server.memoirLLM (by default Qwen 3.8 27B on Cerebras, medium
// reasoning), separate from the one /api/explain uses. There is no per-request
// model choice.

// utf16Len counts s in UTF-16 code units — JavaScript's String.length, which
// is what Cairn measures its Memoir caps in. len(s) counts UTF-8 bytes, which
// turns a 600-character selection of curly quotes into 1800. A rune with no
// UTF-16 form counts as 1 (invalid UTF-8 already decodes to U+FFFD).
func utf16Len(s string) int {
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

// memoirComplete runs one prompt to completion, collecting streamed fragments.
func (s *Server) memoirComplete(r *http.Request, prompt string) (string, llm.Result) {
	var sb strings.Builder
	res := s.memoirLLM.Stream(r.Context(), prompt, func(t string) { sb.WriteString(t) })
	return sb.String(), res
}

// handleMemoirCheck — POST /api/memoir/check {"content": "..."}.
func (s *Server) handleMemoirCheck(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "application/json", isJSON) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "Invalid JSON.", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"corrected": "", "issues": []memoir.CheckIssue{}, "raw": ""})
		return
	}
	if utf16Len(body.Content) > memoir.CheckMaxChars {
		jsonError(w, "entry too long (max 12000 chars)", http.StatusBadRequest)
		return
	}
	if s.memoirLLM == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "CEREBRAS_API_KEY is not set."})
		return
	}

	rawText, res := s.memoirComplete(r, memoir.BuildCheckPrompt(body.Content))
	if !res.OK {
		msg := res.Message
		if msg == "" {
			msg = "Provider error (" + res.Code + ")."
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": msg, "raw": rawText})
		return
	}
	parsed := memoir.TryParseCheckResult(rawText)
	if parsed == nil {
		// Never send unparsed model text back as `corrected`: clients read
		// `corrected` as the fixed entry (Cairn shows it above the warning).
		// Send the author's own text instead; the raw reply stays under `raw`.
		writeJSON(w, http.StatusOK, map[string]any{
			"corrected": body.Content, "issues": []memoir.CheckIssue{},
			"raw": rawText, "warning": "LLM did not return JSON",
		})
		return
	}
	issues := make([]memoir.CheckIssue, 0, len(parsed.Issues))
	for _, it := range parsed.Issues {
		if it.Original == "" || it.Suggestion == "" {
			continue
		}
		it.Category = strings.ToLower(strings.Join(strings.Fields(it.Category), "_"))
		if it.Category == "" {
			it.Category = "grammar"
		}
		issues = append(issues, it)
		if len(issues) >= memoir.CheckMaxIssues {
			break
		}
	}
	corrected := parsed.Corrected
	var warning any
	if !memoir.SanityFloor(corrected, body.Content) {
		corrected = body.Content
		warning = "model returned an implausible correction; showing your original text"
	}
	resp := map[string]any{"corrected": corrected, "issues": issues, "raw": rawText}
	if warning != nil {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMemoirParaphrase — POST /api/memoir/paraphrase {"text": "..."}.
func (s *Server) handleMemoirParaphrase(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "application/json", isJSON) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "Invalid JSON.", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"source": "", "variants": []memoir.Variant{}, "raw": ""})
		return
	}
	if utf16Len(body.Text) > memoir.ParaphraseMaxChars {
		jsonError(w, "selection too long (max 600 chars)", http.StatusBadRequest)
		return
	}
	if s.memoirLLM == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "CEREBRAS_API_KEY is not set."})
		return
	}

	rawText, res := s.memoirComplete(r, memoir.BuildParaphrasePrompt(body.Text))
	if !res.OK {
		msg := res.Message
		if msg == "" {
			msg = "Provider error (" + res.Code + ")."
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": msg, "raw": rawText})
		return
	}
	parsed, ok := memoir.TryParseParaphraseResult(rawText)
	if !ok {
		// An unparseable reply is a failure, not "no alternatives": a 200 with an
		// empty list tells the learner their sentence is already the plainest
		// phrasing (Cairn does not read `warning`), while a non-OK status shows
		// as "Could not rephrase right now." A deliberate {"variants":[]} parses
		// fine and still answers 200 with an empty list below.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "The model returned nothing usable. Try again.", "raw": rawText,
		})
		return
	}
	variants := memoir.NormalizeVariants(parsed, body.Text)
	if variants == nil {
		variants = []memoir.Variant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": body.Text, "variants": variants, "raw": rawText})
}
