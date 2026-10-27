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
func Run(c Controller, onPick func(kind, provider string)) {
	systray.Run(func() { onReady(c, onPick) }, func() {})
}

// Quit ends Run. It is safe to call when the tray never started.
func Quit() {
	systray.Quit()
}

func onReady(c Controller, onPick func(kind, provider string)) {
	systray.SetTemplateIcon(iconTemplate, iconRegular)
	systray.SetTooltip("ai-service — AI provider pickers")

	// One radio-like group per route: checking one item unchecks the others.
	// The menu callbacks run on their own goroutines, so guard the checks.
	var mu sync.Mutex
	for _, sec := range pickerSections(c) {
		sec := sec
		header := systray.AddMenuItem(sec.title, sec.tip)
		header.Disable()
		items := make(map[string]*systray.MenuItem, len(sec.providers))
		for _, provider := range sec.providers {
			items[provider] = systray.AddMenuItemCheckbox(
				providerLabel(sec.kind, provider),
				sec.tip,
				provider == sec.current,
			)
		}
		for _, provider := range sec.providers {
			provider := provider
			go func() {
				for range items[provider].ClickedCh {
					mu.Lock()
					for _, other := range sec.providers {
						if other == provider {
							items[other].Check()
						} else {
							items[other].Uncheck()
						}
					}
					mu.Unlock()
					if setProvider(c, sec.kind, provider) {
						log.Printf("%s provider: %s (tray)", sec.kind, providerLabel(sec.kind, provider))
						if onPick != nil {
							onPick(sec.kind, provider)
						}
					}
				}
			}()
		}
	}
}
