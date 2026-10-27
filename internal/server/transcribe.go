package server

import (
	"net/http"
)

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

// TranscribeProvider returns the provider /api/transcribe/stream is using now.
func (s *Server) TranscribeProvider() string {
	if v, ok := s.transcribeProvider.Load().(string); ok && v != "" {
		return v
	}
	// A Server built by hand in a test has no atomic value; fall back to the
	// configuration it was given.
	return s.cfg.TranscribeProvider
}

// SetTranscribeProvider switches /api/transcribe/stream to provider. It reports
// false for a provider that is not configured, so a stale saved selection or a
// bogus menu click cannot route a recording to a service without a key.
func (s *Server) SetTranscribeProvider(provider string) bool {
	for _, enabled := range s.transcribeProviders {
		if provider == enabled {
			s.transcribeProvider.Store(provider)
			return true
		}
	}
	return false
}
