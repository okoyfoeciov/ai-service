package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"ai-service/internal/config"
)

// maxTranscribeBytes is the per-clip ceiling for /api/transcribe. It matches
// the streamed route's cap.
const maxTranscribeBytes = 25 * 1024 * 1024

// extByType maps a recording's MIME type to a file extension the STT backend
// recognises.
var extByType = [][2]string{
	{"mp4", "mp4"},
	{"m4a", "m4a"},
	{"mpeg", "mp3"},
	{"ogg", "ogg"},
	{"flac", "flac"},
	{"wav", "wav"},
	{"webm", "webm"},
	{"aac", "aac"},
}

func pickExt(contentType string) string {
	for _, pair := range extByType {
		if strings.Contains(contentType, pair[0]) {
			return pair[1]
		}
	}
	return "webm"
}

func (s *Server) shortClient() *http.Client {
	if s.httpShort != nil {
		return s.httpShort
	}
	return http.DefaultClient
}

// headerOrNil is the header's value, or nil (JSON null) when it is absent.
func headerOrNil(h http.Header, name string) any { return nullableString(h.Get(name)) }

// nullableString is s, or nil (JSON null) when it is empty.
func nullableString(s string) any {
	if s != "" {
		return s
	}
	return nil
}

// TranscribeProviders returns the providers config.Load found, in the order
// the tray menu shows them. The slice is a copy.
func (s *Server) TranscribeProviders() []string {
	return append([]string(nil), s.transcribeProviders...)
}

// TranscribeProvider returns the provider /api/transcribe is using now.
func (s *Server) TranscribeProvider() string {
	if v, ok := s.transcribeProvider.Load().(string); ok && v != "" {
		return v
	}
	// A Server built by hand in a test has no atomic value; fall back to the
	// configuration it was given.
	return s.cfg.TranscribeProvider
}

// SetTranscribeProvider switches /api/transcribe to provider. It reports false
// for a provider that is not configured, so a stale saved selection or a bogus
// menu click cannot route a recording to a service without a key.
func (s *Server) SetTranscribeProvider(provider string) bool {
	for _, enabled := range s.transcribeProviders {
		if provider == enabled {
			s.transcribeProvider.Store(provider)
			return true
		}
	}
	return false
}

// handleTranscribe sends one clip from comart's mic recorder (Exscriptor) to
// the selected /api/transcribe provider and answers {"text": ...}.
func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "an audio type", func(mt string) bool {
		return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/")
	}) {
		return
	}
	provider := s.TranscribeProvider()
	if provider == "" {
		jsonError(w, "server not configured: ASSEMBLYAI_API_KEY missing", http.StatusInternalServerError)
		return
	}
	contentType := r.Header.Get("Content-Type")

	r.Body = http.MaxBytesReader(w, r.Body, maxTranscribeBytes)
	audio, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, "recording too long", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "could not read body", http.StatusBadRequest)
		return
	}
	if len(audio) < 1200 {
		jsonError(w, "recording too short", http.StatusUnprocessableEntity)
		return
	}

	switch provider {
	case config.ProviderAssemblyAI:
		s.transcribeAssemblyAI(w, r, audio, pickExt(contentType))
	default:
		jsonError(w, "server not configured", http.StatusInternalServerError)
	}
}
