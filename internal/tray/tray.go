// Package tray is the /api/transcribe provider picker shown in the desktop's
// menu bar / status area. It exists only when both Groq and Azure
// MAI-Transcribe-2 are configured: with one provider there is nothing to pick.
//
// The tray is not part of the HTTP API. It runs in the service process and
// flips the same provider field the transcribe handler reads, so comart keeps
// calling POST /api/transcribe unchanged.
package tray

import (
	_ "embed"
	"log"
	"os"
	"runtime"
	"sync"

	"fyne.io/systray"

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

// Run shows the tray icon and blocks in the platform's GUI event loop until
// Quit is called. Call it from the main goroutine: macOS runs the Cocoa loop
// there.
func Run(c Controller, onPick func(string)) {
	// A Linux service can run without a desktop session (SSH, early boot). The
	// HTTP server is still useful there; the picker is not, and systray would
	// otherwise block forever waiting for a menu it cannot show.
	if runtime.GOOS == "linux" && os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		log.Printf("tray: no session D-Bus available — provider picker needs a desktop session")
		return
	}
	systray.Run(func() { onReady(c, onPick) }, func() {})
}

// Quit ends Run. It is safe to call when the tray never started.
func Quit() {
	systray.Quit()
}

func onReady(c Controller, onPick func(string)) {
	systray.SetTemplateIcon(iconTemplate, iconRegular)
	systray.SetTooltip("ai-service — transcription provider")

	providers := c.TranscribeProviders()
	current := c.TranscribeProvider()

	header := systray.AddMenuItem("Transcribe with", "The provider /api/transcribe uses")
	header.Disable()
	items := make(map[string]*systray.MenuItem, len(providers))
	for _, p := range providers {
		items[p] = systray.AddMenuItemCheckbox(providerLabel(p), "Use "+providerLabel(p)+" for /api/transcribe", p == current)
	}

	// One radio-like group: checking one item unchecks the others. The menu
	// callbacks run on their own goroutines, so guard the checks with a mutex.
	var mu sync.Mutex
	for _, p := range providers {
		p := p
		go func() {
			for range items[p].ClickedCh {
				mu.Lock()
				for _, other := range providers {
					if other == p {
						items[other].Check()
					} else {
						items[other].Uncheck()
					}
				}
				mu.Unlock()
				if c.SetTranscribeProvider(p) {
					log.Printf("transcribe provider: %s (tray)", providerLabel(p))
					if onPick != nil {
						onPick(p)
					}
				}
			}
		}()
	}
}

func providerLabel(provider string) string {
	switch provider {
	case config.ProviderGroq:
		return "Groq Whisper"
	case config.ProviderMAITranscribe:
		return "MAI-Transcribe-2 (Azure)"
	}
	return provider
}
