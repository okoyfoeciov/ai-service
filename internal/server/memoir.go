package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"

	"ai-service/internal/llm"
	"ai-service/internal/memoir"
)

// Memoir handlers for Cairn's Memoir tab and comart's mic card: proofread an
// entry (check). The limits and response shapes are the clients' contract;
// keep them stable. Unparsed model text is never handed back as a correction.
//
// Unlike /api/explain this is non-streaming: the client renders the finished
// result, so the handler collects the stream and responds once. The clients
// are Server.memoirLLMs, separate from /api/explain's; the tray picker chooses
// which one answers, and there is no per-request model choice.

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

// MemoirProviders returns the configured Memoir providers, in the order the
// tray menu shows them. The slice is a copy.
func (s *Server) MemoirProviders() []string {
	return append([]string(nil), s.memoirProviders...)
}

// MemoirProvider returns the provider /api/memoir/check is using now.
func (s *Server) MemoirProvider() string {
	if v, ok := s.memoirProvider.Load().(string); ok && v != "" {
		return v
	}
	// A Server built by hand in a test has no atomic value; fall back to the
	// configuration it was given.
	return s.cfg.MemoirProvider
}

// SetMemoirProvider switches /api/memoir/check to provider. It reports false
// for a provider that is not configured, so a stale saved selection or a bogus
// menu click cannot route a check to a service without a key.
func (s *Server) SetMemoirProvider(provider string) bool {
	for _, enabled := range s.memoirProviders {
		if provider == enabled {
			s.memoirProvider.Store(provider)
			return true
		}
	}
	return false
}

// memoirComplete runs one prompt to completion, collecting streamed fragments.
func memoirComplete(r *http.Request, streamer llm.Streamer, prompt string) (string, llm.Result) {
	var sb strings.Builder
	res := streamer.Stream(r.Context(), prompt, func(t string) { sb.WriteString(t) })
	return sb.String(), res
}

// handleMemoirCheck — POST /api/memoir/check {"content": "..."}.
func (s *Server) handleMemoirCheck(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// Resolve the tray choice once, so a switch mid-request cannot pair this
	// request's log line with another provider's model.
	provider := s.MemoirProvider()
	streamer := s.memoirLLMs[provider]
	model := s.cfg.MemoirModelFor(provider)
	sw := &statusWriter{ResponseWriter: w}
	w = sw
	outcome := "ok"
	issueCount := 0
	defer func() {
		providerName, modelName := provider, model
		if providerName == "" {
			providerName = "none"
		}
		if modelName == "" {
			modelName = "-"
		}
		log.Printf("memoir[%s]: model=%s status=%d outcome=%s issues=%d duration=%s",
			providerName, modelName, sw.Status(), outcome, issueCount, time.Since(start).Round(time.Millisecond))
	}()

	if !requireType(w, r, "application/json", isJSON) {
		outcome = "bad_request"
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		outcome = "bad_request"
		jsonError(w, "Invalid JSON.", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		outcome = "empty"
		writeJSON(w, http.StatusOK, map[string]any{"corrected": "", "issues": []memoir.CheckIssue{}, "raw": ""})
		return
	}
	if utf16Len(body.Content) > memoir.CheckMaxChars {
		outcome = "too_long"
		jsonError(w, fmt.Sprintf("entry too long (max %d chars)", memoir.CheckMaxChars), http.StatusBadRequest)
		return
	}
	if streamer == nil {
		outcome = "no_key"
		jsonError(w, "Neither MEMOIR_CLAUDE_API_KEY nor MEMOIR_OPENROUTER_API_KEY is set.", http.StatusServiceUnavailable)
		return
	}

	rawText, res := memoirComplete(r, streamer, memoir.BuildCheckPrompt(body.Content))
	if !res.OK {
		outcome = "upstream_error"
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": res.UserMessage(), "raw": rawText})
		return
	}
	parsed := memoir.TryParseCheckResult(rawText)
	if parsed == nil {
		outcome = "unparsed"
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
	issueCount = len(issues)
	corrected := parsed.Corrected
	var warning any
	if !memoir.SanityFloor(corrected, body.Content) {
		outcome = "implausible"
		corrected = body.Content
		warning = "model returned an implausible correction; showing your original text"
	}
	resp := map[string]any{"corrected": corrected, "issues": issues, "raw": rawText}
	if warning != nil {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}
