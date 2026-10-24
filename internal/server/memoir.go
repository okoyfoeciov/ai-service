package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf16"

	"ai-service/internal/llm"
	"ai-service/internal/memoir"
)

// Memoir handlers for Cairn's Memoir tab: proofread an entry (check). The
// limits and response shapes are what Cairn's src/memoir.ts expects; keep them
// stable. Unparsed model text is never handed back as a correction.
//
// Unlike /api/explain this is non-streaming JSON: the client renders the
// finished result, so the handler collects the stream and responds once.
// The model is Server.memoirLLM (Claude Opus 5.5 at medium effort by default,
// or Qwen 3.8 27B on Cerebras when MEMOIR_CEREBRAS_API_KEY is set instead),
// separate from the one /api/explain uses. There is no per-request model
// choice.

// utf16Len counts s in UTF-16 code units — JavaScript's String.length, which
// is what Cairn measures its Memoir cap in. len(s) counts UTF-8 bytes, which
// turns a 12000-character entry of curly quotes into 36000. A rune with no
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
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Neither MEMOIR_CEREBRAS_API_KEY nor MEMOIR_CLAUDE_API_KEY is set."})
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
