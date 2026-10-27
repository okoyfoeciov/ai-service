// Package tray is the /api/transcribe provider picker shown in the desktop's
// menu bar / status area. It exists only when more than one transcription
// provider is configured: with one provider there is nothing to pick.
//
// The tray is not part of the HTTP API. It runs in the service process and
// flips the same provider field the transcribe handler reads, so comart keeps
// calling POST /api/transcribe unchanged.
package tray

import (
	_ "embed"

	"ai-service/internal/config"
)

// The icons are embedded: the service has no assets directory and must keep
// working when the repository is moved.
var (
	//go:embed icon.png
	iconRegular []byte
	//go:embed icon-template.png
	iconTemplate []byte
)

// Controller is the part of *server.Server the picker needs.
type Controller interface {
	TranscribeProviders() []string
	TranscribeProvider() string
	SetTranscribeProvider(string) bool
}

func providerLabel(provider string) string {
	switch provider {
	case config.ProviderAssemblyAI:
		return "AssemblyAI Universal-3.5 Pro"
	}
	return provider
}
