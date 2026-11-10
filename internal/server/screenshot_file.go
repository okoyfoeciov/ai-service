package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// readScreenshotFile reads one capture the platform saved to disk and removes
// it. Both the Linux portal and macOS screencapture end here, so the checks
// and the delete-on-every-path rule live in one place.
func readScreenshotFile(path string) ([]byte, error) {
	// Delete on every path after this point, including a failed read or an
	// oversized file. Leaving a PNG behind is a privacy/disk leak.
	defer func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("screenshot: remove %s: %v", path, err)
		}
	}()
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("screenshot file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("screenshot is not a regular file")
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
		return nil, fmt.Errorf("screenshot type %q", ext)
	}
	const maxRaw = 64 << 20
	if info.Size() > maxRaw {
		return nil, fmt.Errorf("screenshot file is %d bytes", info.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read screenshot: %w", err)
	}
	return raw, nil
}
