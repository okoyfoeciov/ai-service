//go:build darwin

package server

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeScreencapture installs a shell script in place of /usr/sbin/screencapture
// and points TMPDIR at a fresh directory so a test can assert nothing is left
// behind. The script runs body with $out set to the output path (its last
// argument); every call appends its arguments to the returned args file.
func fakeScreencapture(t *testing.T, body string) (tmp, argsFile string) {
	t.Helper()
	tmp = t.TempDir()
	t.Setenv("TMPDIR", tmp)
	argsFile = filepath.Join(t.TempDir(), "args")
	script := filepath.Join(t.TempDir(), "screencapture")
	src := "#!/bin/bash\n" +
		"echo \"$*\" >> " + shellQuote(argsFile) + "\n" +
		"for out; do :; done\n" +
		body + "\n"
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	old := screencaptureBin
	screencaptureBin = script
	t.Cleanup(func() { screencaptureBin = old })
	return tmp, argsFile
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func writeTestPNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// requireEmptyDir fails if dir has anything in it: no capture directory and no
// PNG may survive a capture, whatever way it ended.
func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("left behind in %s: %v", dir, names)
	}
}

func TestCaptureRegionOneDragSavesAndCleansUp(t *testing.T) {
	fixture := writeTestPNG(t, 40, 30)
	tmp, argsFile := fakeScreencapture(t, `cp `+shellQuote(fixture)+` "$out"`)

	raw, err := captureRegion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width != 40 || cfg.Height != 30 {
		t.Fatalf("decoded %+v, %v", cfg, err)
	}
	args, _ := os.ReadFile(argsFile)
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 1 {
		t.Fatalf("screencapture ran %d times, want exactly 1 (one drag): %q", len(lines), lines)
	}
	// -i -s is the one-drag contract: -s stops Space from switching to window
	// mode, and nothing asks for a delay, a timer or a confirm step.
	for _, want := range []string{"-i", "-s", "-x", "-t png"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("args %q missing %q", lines[0], want)
		}
	}
	for _, bad := range []string{"-T", "-w", "-W", "-c", "-P", "-U"} {
		for _, field := range strings.Fields(lines[0]) {
			if field == bad {
				t.Errorf("args %q contain %q", lines[0], bad)
			}
		}
	}
	requireEmptyDir(t, tmp)
}

func TestCaptureRegionCaptureDirIsPrivate(t *testing.T) {
	var seen os.FileMode
	fixture := writeTestPNG(t, 4, 4)
	tmp, _ := fakeScreencapture(t, `cp `+shellQuote(fixture)+` "$out"; echo extra > "$(dirname "$out")/extra"`)
	// Read the mode from inside the call by wrapping runScreencapture.
	real := runScreencapture
	runScreencapture = func(ctx context.Context, path string) error {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			return err
		}
		seen = info.Mode().Perm()
		return real(ctx, path)
	}
	t.Cleanup(func() { runScreencapture = real })
	// The fake also writes a stray file next to the PNG; RemoveAll must take
	// it with the directory.
	if _, err := captureRegion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen != 0o700 {
		t.Fatalf("capture dir mode %o, want 700", seen)
	}
	requireEmptyDir(t, tmp)
}

func TestCaptureRegionCancelledSelectionLeavesNothing(t *testing.T) {
	// Escape (or Control, which copies to the clipboard) exits 0, no file.
	tmp, _ := fakeScreencapture(t, `exit 0`)
	_, err := captureRegion(context.Background())
	if !errors.Is(err, errScreenshotCancelled) {
		t.Fatalf("err = %v, want cancelled", err)
	}
	requireEmptyDir(t, tmp)
}

func TestCaptureRegionFailureReportsStderrAndCleansUp(t *testing.T) {
	tmp, _ := fakeScreencapture(t, `echo "could not create image from display" >&2; touch "$out"; exit 1`)
	_, err := captureRegion(context.Background())
	if err == nil || errors.Is(err, errScreenshotCancelled) {
		t.Fatalf("err = %v, want a real failure", err)
	}
	if !strings.Contains(err.Error(), "could not create image") {
		t.Fatalf("err = %v, want screencapture's stderr", err)
	}
	requireEmptyDir(t, tmp)
}

func TestCaptureRegionBadFilesAreRejectedAndRemoved(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		tmp, _ := fakeScreencapture(t, `: > "$out"`)
		// An empty file is passed on (prepareScreenshot rejects it later). The
		// file and its directory must still be gone.
		raw, err := captureRegion(context.Background())
		if err == nil {
			if _, _, _, _, _, perr := prepareScreenshot(raw); perr == nil {
				t.Fatal("an empty capture was accepted end to end")
			}
		}
		requireEmptyDir(t, tmp)
	})
	t.Run("symlink", func(t *testing.T) {
		target := writeTestPNG(t, 4, 4)
		tmp, _ := fakeScreencapture(t, `ln -s `+shellQuote(target)+` "$out"`)
		_, err := captureRegion(context.Background())
		if err == nil {
			t.Fatal("a symlink was accepted as a capture")
		}
		requireEmptyDir(t, tmp)
		if _, statErr := os.Stat(target); statErr != nil {
			t.Fatalf("the symlink's target was deleted: %v", statErr)
		}
	})
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still running", pid)
}

func TestCaptureRegionRequestEndKillsScreencaptureAndCleansUp(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	tmp, _ := fakeScreencapture(t, `echo $$ > `+shellQuote(pidFile)+`; touch "$out"; exec sleep 60`)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := captureRegion(ctx); errc <- err }()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("fake screencapture never started")
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("captureRegion did not return after the request ended")
	}
	waitGone(t, pid)
	requireEmptyDir(t, tmp)

	// The slot must be free again, or one dropped request would block every
	// later capture until restart.
	select {
	case captureSlot <- struct{}{}:
		<-captureSlot
	default:
		t.Fatal("capture slot still held after cancel")
	}
}

func TestCaptureRegionKillsScreencaptureThatIgnoresSIGTERM(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// A screencapture stuck in an uninterruptible UI call would ignore TERM.
	tmp, _ := fakeScreencapture(t, `trap '' TERM; echo $$ > `+shellQuote(pidFile)+`; while :; do sleep 0.1; done`)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := captureRegion(ctx); errc <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-errc:
	case <-time.After(screencaptureKillDelay + 8*time.Second):
		t.Fatal("captureRegion hung on a process that ignores SIGTERM")
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		t.Fatal("no pid recorded")
	}
	waitGone(t, pid)
	requireEmptyDir(t, tmp)
}

func TestCaptureRegionIsSingleFlight(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	started := filepath.Join(t.TempDir(), "started")
	fixture := writeTestPNG(t, 4, 4)
	tmp, argsFile := fakeScreencapture(t, `touch `+shellQuote(started)+`; while [ ! -e `+shellQuote(release)+` ]; do sleep 0.02; done; cp `+shellQuote(fixture)+` "$out"`)

	first := make(chan error, 1)
	go func() { _, err := captureRegion(context.Background()); first <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first capture never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := captureRegion(context.Background()); !errors.Is(err, errScreenshotBusy) {
		t.Fatalf("second capture err = %v, want busy", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first capture: %v", err)
	}
	args, _ := os.ReadFile(argsFile)
	if n := strings.Count(string(args), "\n"); n != 1 {
		t.Fatalf("screencapture ran %d times, want 1 (the busy call must not spawn it)", n)
	}
	requireEmptyDir(t, tmp)
}

func TestSweepStaleScreenshotDirs(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, screenshotDirPrefix+"old")
	fresh := filepath.Join(root, screenshotDirPrefix+"fresh")
	other := filepath.Join(root, "someone-else")
	for _, d := range []string{old, fresh, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(old, "capture.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	for _, d := range []string{old, other} {
		if err := os.Chtimes(d, past, past); err != nil {
			t.Fatal(err)
		}
	}
	sweepStaleScreenshotDirs(root, time.Now().Add(-screenshotStaleAge))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale capture dir survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a live capture dir was swept: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a directory without our prefix was swept: %v", err)
	}
}

func TestCaptureRegionDoesNotLeakGoroutines(t *testing.T) {
	fixture := writeTestPNG(t, 8, 8)
	fakeScreencapture(t, `cp `+shellQuote(fixture)+` "$out"`)
	// Warm up lazily started runtime goroutines first.
	if _, err := captureRegion(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		if _, err := captureRegion(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var after int
	for i := 0; i < 50; i++ {
		after = runtime.NumGoroutine()
		if after <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines grew from %d to %d over 20 captures", before, after)
}

func TestScreenshotRouteOnMacOS(t *testing.T) {
	fixture := writeTestPNG(t, 64, 48)
	tmp, _ := fakeScreencapture(t, `cp `+shellQuote(fixture)+` "$out"`)
	srv := &Server{shots: newScreenshotStore(time.Hour, 8<<20, 8)}
	req := httptest.NewRequest(http.MethodPost, "/api/screenshot", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:8770"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id"`) || !strings.Contains(rec.Body.String(), `"width":64`) {
		t.Fatalf("body %s", rec.Body.String())
	}
	requireEmptyDir(t, tmp)
}

func TestScreenshotRouteBusyIsNot409(t *testing.T) {
	// comart treats 409 as "the reader cancelled" and shows nothing.
	captureSlot <- struct{}{}
	t.Cleanup(func() { <-captureSlot })
	srv := &Server{shots: newScreenshotStore(time.Hour, 8<<20, 8)}
	req := httptest.NewRequest(http.MethodPost, "/api/screenshot", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:8770"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
}
