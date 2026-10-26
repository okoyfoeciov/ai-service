// Command ai-service is the localhost LLM backend for the comart Chrome
// extension (word explanations, pronunciations, mic transcription), Cairn's
// Memoir tab (proofreading) and daily-dictation (verbatim transcription). It
// binds to loopback and nothing fronts it, so loopback is the trust boundary.
// run.sh starts it under the launchd agent (macOS) or systemd user unit
// (Linux) that install.sh writes.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"ai-service/internal/config"
	"ai-service/internal/server"
	"ai-service/internal/store"
	"ai-service/internal/tray"
)

// savedTranscribeProviderKey is the cache row the tray picker writes, so the
// chosen provider survives a restart. It never expires.
const savedTranscribeProviderKey = "transcribe:provider"

func main() {
	cfg, missing, err := config.Load()
	if err != nil {
		// A configuration that makes the model routing unclear is not run at
		// all: the service manager keeps retrying and the reason is in the log.
		log.Fatalf("config: %v", err)
	}
	for _, m := range missing {
		log.Printf("WARNING: %s is missing — its feature reports an error until you add it and restart the service", m)
	}

	dbPath := os.Getenv("CACHE_DB")
	// run.sh always sets CACHE_DB (by default to data/cache.db in its own
	// directory); this fallback only matters for a binary started by hand, and
	// assumes ~/ai-service.
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dbPath = filepath.Join(home, "ai-service", "data", "cache.db")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open cache store: %v", err)
	}
	defer st.Close()

	// SIGINT/SIGTERM cancels ctx, which starts the HTTP shutdown below.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app := server.New(cfg, st)

	// A saved tray choice wins over the config default, but only when that
	// provider still has a key: removing a key must not leave /api/transcribe
	// pointing at a service that is no longer configured.
	multiProvider := len(cfg.TranscribeProviders) > 1
	if multiProvider {
		if saved, ok := st.Get(savedTranscribeProviderKey); ok && app.SetTranscribeProvider(saved) {
			cfg.TranscribeProvider = saved
			log.Printf("transcribe provider: %s (saved by the tray picker)", saved)
		}
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second, // slowloris guard on headers only
		// No WriteTimeout: it would cut off a long SSE stream or a slow Memoir
		// answer. No ReadTimeout either: each handler caps its own body size
		// (5 MiB JSON, 25 MiB /api/transcribe, 300 MiB verbatim) instead.
	}

	// Graceful shutdown: drain in-flight requests, then close the store, which
	// checkpoints the WAL. Merriam-Webster lookups that are still running
	// after their request ends are not waited for; at worst their cache write
	// is lost. The two budgets (10 s + 5 s) stay under launchd's default 20 s
	// exit timeout; systemd allows 90 s.
	var trayActive atomic.Bool
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		stop() // a second SIGINT/SIGTERM now kills the process at once
		log.Println("shutting down…")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			// Handlers still running (a long SSE stream): cut them off so their
			// model calls abort.
			_ = srv.Close()
		}
		// Close returns before the handlers it cut off have unwound; wait for
		// them before the store closes. Its own budget, not what is left of
		// Shutdown's.
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		app.Drain(dctx)
		// macOS keeps the process in Cocoa's event loop; the tray must quit
		// before main can return and close the store.
		if trayActive.Load() {
			tray.Quit()
		}
	}()

	log.Printf("transcribe route: %s", cfg.TranscribeRoute())
	log.Printf("explain route: %s", cfg.ExplainRoute())
	log.Printf("memoir route: %s", cfg.MemoirRoute())
	log.Printf("ai-service listening on http://%s  (cache: %s)", cfg.ListenAddr, dbPath)

	// ListenAndServe blocks, so it runs beside the tray event loop. On macOS
	// systray.Run must own the main goroutine (Cocoa); on Linux it waits on
	// D-Bus. When only one provider is configured there is nothing to pick, so
	// the process simply waits for the signal above.
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()
	if multiProvider {
		log.Printf("tray: both transcription providers are configured; the picker is in the menu bar")
		trayActive.Store(true)
		tray.Run(app, func(provider string) {
			st.Set(savedTranscribeProviderKey, provider, 0)
		})
		trayActive.Store(false)
	}
	// Shutdown returns as soon as Shutdown starts, not when it ends; wait for
	// the drain before the deferred st.Close() and the process exit.
	<-shutdownDone
	log.Println("stopped")
}
