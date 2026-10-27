// Package tray is the provider picker shown in the desktop's menu bar / status
// area. It has one section per route with more than one provider; with one
// provider on both routes there is nothing to pick and the tray does not run.
//
// The tray is not part of the HTTP API. It runs in the service process and
// flips the same provider fields the handlers read, so clients keep calling the
// routes unchanged.
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

	// Exact-size pixmaps for the Linux StatusNotifier host. Supplying these
	// avoids scaling one large raster down to a fuzzy 16/22 px tray glyph.
	//go:embed icon-16.png
	icon16 []byte
	//go:embed icon-22.png
	icon22 []byte
	//go:embed icon-24.png
	icon24 []byte
	//go:embed icon-32.png
	icon32 []byte
	//go:embed icon-44.png
	icon44 []byte
	//go:embed icon-48.png
	icon48 []byte
)

// Pick kinds, also used as the cache-key prefix by main.
const (
	PickTranscribe = "transcribe"
	PickExplain    = "explain"
)

// Controller is the part of *server.Server the pickers need.
type Controller interface {
	TranscribeProviders() []string
	TranscribeProvider() string
	SetTranscribeProvider(string) bool
	ExplainProviders() []string
	ExplainProvider() string
	SetExplainProvider(string) bool
}

// section is one picker group in the menu.
type section struct {
	kind      string
	title     string
	tip       string
	providers []string
	current   string
}

// pickerSections returns only the routes with more than one configured
// provider, in menu order.
func pickerSections(c Controller) []section {
	var sections []section
	if providers := c.TranscribeProviders(); len(providers) > 1 {
		sections = append(sections, section{
			kind:      PickTranscribe,
			title:     "Transcribe with",
			tip:       "The provider /api/transcribe/stream uses",
			providers: providers,
			current:   c.TranscribeProvider(),
		})
	}
	if providers := c.ExplainProviders(); len(providers) > 1 {
		sections = append(sections, section{
			kind:      PickExplain,
			title:     "Explain with",
			tip:       "The provider /api/explain uses",
			providers: providers,
			current:   c.ExplainProvider(),
		})
	}
	return sections
}

func providerLabel(kind, provider string) string {
	switch kind {
	case PickTranscribe:
		switch provider {
		case config.ProviderAssemblyAI:
			return "AssemblyAI Universal-3.5 Pro"
		case config.ProviderDeepgram:
			return "Deepgram Nova-3"
		}
	case PickExplain:
		switch provider {
		case config.ProviderClaude:
			return "Claude Platform"
		case config.ProviderOpenRouter:
			return "OpenRouter"
		}
	}
	return provider
}

func setProvider(c Controller, kind, provider string) bool {
	switch kind {
	case PickTranscribe:
		return c.SetTranscribeProvider(provider)
	case PickExplain:
		return c.SetExplainProvider(provider)
	}
	return false
}
