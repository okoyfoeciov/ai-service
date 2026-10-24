package server

import (
	"log"
	"net/http"
)

// handlePron — Merriam-Webster pronunciation lookup.
func (s *Server) handlePron(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	word := q.Get("word")
	pos := q.Get("pos")

	result, err := s.mw.Lookup(word, pos)
	if err != nil {
		// MW refused the request (challenge / 5xx / network) — an upstream
		// outage, distinct from a word with no pronunciation.
		log.Printf("/api/pron: %v", err)
		jsonError(w, "mw_unavailable", http.StatusBadGateway)
		return
	}
	if result == nil {
		// No pronunciation for this word: MW's page has none, MW has no page
		// for it (404), or the word is empty.
		jsonError(w, "not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleCredits always answers "not low": the server does not track a
// balance. comart still pings this route after each explain and ignores the
// answer; the route stays so that ping does not fail.
func (s *Server) handleCredits(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"remaining": nil, "low": false})
}
