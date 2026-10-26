//go:build darwin

package tray

import (
	"log"
	"sync"

	"fyne.io/systray"
)

// Run shows the tray icon and blocks in the platform's GUI event loop until
// Quit is called. Call it from the main goroutine: macOS runs the Cocoa loop
// there.
func Run(c Controller, onPick func(string)) {
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
