//go:build darwin

package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// screencaptureTimeout bounds one selection. It matches the Linux portal.
	screencaptureTimeout = 2 * time.Minute
	// screencaptureKillDelay is how long screencapture gets to exit after
	// SIGTERM before Go sends SIGKILL, so Wait can never hang.
	screencaptureKillDelay = 3 * time.Second
	// screenshotDirPrefix names the private directory each capture uses.
	screenshotDirPrefix = "ai-service-shot-"
	// screenshotStaleAge is when a leftover directory (the service was killed
	// mid-capture) is swept. It is well above screencaptureTimeout, so a live
	// capture is never swept.
	screenshotStaleAge = 10 * time.Minute
)

// screencaptureBin is a variable so a test can run a fake instead of putting
// a selection overlay on the owner's screen.
var screencaptureBin = "/usr/sbin/screencapture"

// captureSlot makes capture single-flight. A second request while a selection
// overlay is open would stack another overlay on the first.
var captureSlot = make(chan struct{}, 1)

// captureRegion runs macOS's own interactive area selection once. The reader
// drags a rectangle and screencapture saves it on mouse release: one drag, no
// confirm step. The PNG goes to a private 0700 directory that is removed on
// every path, then the bytes are read like the Linux portal's file.
func captureRegion(ctx context.Context) ([]byte, error) {
	select {
	case captureSlot <- struct{}{}:
		defer func() { <-captureSlot }()
	default:
		return nil, errScreenshotBusy
	}

	ctx, cancel := context.WithTimeout(ctx, screencaptureTimeout)
	defer cancel()

	tmp := os.TempDir()
	sweepStaleScreenshotDirs(tmp, time.Now().Add(-screenshotStaleAge))
	dir, err := os.MkdirTemp(tmp, screenshotDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("screenshot temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("screenshot: remove %s: %v", dir, err)
		}
	}()
	path := filepath.Join(dir, "capture.png")

	if err := runScreencapture(ctx, path); err != nil {
		return nil, err
	}
	// Escape, or Control (which sends the shot to the clipboard instead),
	// ends screencapture cleanly with no file: the reader changed their mind.
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, errScreenshotCancelled
	}
	return readScreenshotFile(path)
}

// runScreencapture is a variable so a test can replace the process hop; the
// directory handling and file checks around it stay real.
//
//	-i  interactive selection
//	-s  mouse selection only: Space cannot switch to window mode, so the
//	    capture is always the dragged rectangle
//	-x  no shutter sound
var runScreencapture = func(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, screencaptureBin, "-i", "-s", "-x", "-t", "png", path)
	stderr := &cappedBuffer{max: 512}
	cmd.Stderr = stderr
	// Ask nicely first so screencapture tears its overlay down, and force it
	// if it does not go: a cancelled request must never leave a process or an
	// overlay behind.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = screencaptureKillDelay
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return fmt.Errorf("screencapture: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// sweepStaleScreenshotDirs removes capture directories older than cutoff that
// a killed service could not clean up.
func sweepStaleScreenshotDirs(root string, cutoff time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), screenshotDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			log.Printf("screenshot: sweep %s: %v", e.Name(), err)
		}
	}
}

// cappedBuffer keeps the first max bytes written and discards the rest, so a
// chatty child cannot grow memory.
type cappedBuffer struct {
	max int
	buf []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return string(c.buf) }
