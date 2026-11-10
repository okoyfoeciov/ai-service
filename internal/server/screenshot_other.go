//go:build !linux && !darwin

package server

import "context"

// captureRegion is available only on Linux and macOS. The route exists on every build so
// the client gets a clear 501 instead of a 404.
func captureRegion(context.Context) ([]byte, error) {
	return nil, errScreenshotUnsupported
}
