package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"time"

	xdraw "golang.org/x/image/draw"
)

// Screenshot lifetime and bounds. The explicit release from the extension is
// the normal path; the TTL is the backstop for a closed tab, a crashed frame
// or an extension reload, and the caps stop a runaway from filling memory.
const (
	screenshotTTL        = 2 * time.Hour
	screenshotMaxBytes   = 128 << 20
	screenshotMaxShots   = 64
	screenshotMaxEdge    = 1200
	screenshotThumbEdge  = 320
	screenshotPNGMaxSize = 512 << 10
)

var (
	// errScreenshotCancelled is the user closing or cancelling the selection UI.
	errScreenshotCancelled = errors.New("screenshot cancelled")
	// errScreenshotBusy is a second capture while a selection is still open.
	errScreenshotBusy = errors.New("a screenshot is already being taken")
	// errScreenshotUnsupported is a platform without a capture backend.
	errScreenshotUnsupported = errors.New("screen capture is only supported on Linux and macOS")
)

// handleScreenshot asks the desktop for one interactive region capture, stores
// the image under an id, and returns only the id and a small preview. The
// extension never receives the full bytes, so /api/ask stays a small JSON
// request and the image cannot leak through message passing.
func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	w = sw
	outcome := "ok"
	defer func() {
		log.Printf("screenshot: status=%d outcome=%s duration=%s",
			sw.Status(), outcome, time.Since(start).Round(time.Millisecond))
	}()
	if !requireType(w, r, "application/json", isJSON) {
		outcome = "bad_content_type"
		return
	}
	// The body is empty by design; enforce a tiny cap and still report a
	// malformed or oversized body instead of opening the portal for it.
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			outcome = "too_large"
			jsonError(w, "Request too large.", http.StatusRequestEntityTooLarge)
			return
		}
		if !errors.Is(err, io.EOF) {
			outcome = "invalid_json"
			jsonError(w, "Invalid JSON.", http.StatusBadRequest)
			return
		}
	}

	raw, err := captureRegion(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, errScreenshotCancelled):
			outcome = "cancelled"
			jsonError(w, "Screenshot cancelled.", http.StatusConflict)
		case errors.Is(err, errScreenshotBusy):
			outcome = "busy"
			// Not 409: comart reads every 409 as the reader cancelling and stays
			// silent, which would hide this from them.
			jsonError(w, "A screenshot is already being taken.", http.StatusTooManyRequests)
		case errors.Is(err, errScreenshotUnsupported):
			outcome = "unsupported"
			jsonError(w, err.Error(), http.StatusNotImplemented)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			outcome = "client_gone"
		default:
			outcome = "error"
			log.Printf("screenshot: %v", err)
			jsonError(w, "Screen capture failed. Try again.", http.StatusInternalServerError)
		}
		return
	}
	if r.Context().Err() != nil {
		outcome = "client_gone"
		return
	}
	full, mediaType, width, height, thumb, err := prepareScreenshot(raw)
	if err != nil {
		outcome = "bad_image"
		log.Printf("screenshot: prepare: %v", err)
		jsonError(w, "The screenshot could not be read.", http.StatusInternalServerError)
		return
	}
	if r.Context().Err() != nil {
		outcome = "client_gone"
		return
	}
	if s.shots == nil {
		outcome = "no_store"
		jsonError(w, "Screenshot storage is unavailable.", http.StatusInternalServerError)
		return
	}
	id, err := s.shots.put(full, mediaType)
	if err != nil {
		outcome = "store_error"
		log.Printf("screenshot: store: %v", err)
		switch {
		case errors.Is(err, errScreenshotBig):
			jsonError(w, "The screenshot is too large.", http.StatusRequestEntityTooLarge)
		case errors.Is(err, errScreenshotFull):
			jsonError(w, "Too many screenshots are being kept. Close an Ask card and try again.", http.StatusInsufficientStorage)
		default:
			jsonError(w, "Could not keep the screenshot.", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"thumb":  thumb,
		"width":  width,
		"height": height,
	})
}

// handleScreenshotRelease deletes the images the extension no longer needs.
// It is called when a non-Ask Stop discards a take and when the Ask card (the
// parent of every round) is dismissed with all its rounds.
func (s *Server) handleScreenshotRelease(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "application/json", isJSON) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, "Request too large.", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "Invalid JSON.", http.StatusBadRequest)
		return
	}
	if s.shots != nil {
		s.shots.release(body.IDs)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// prepareScreenshot bounds and re-encodes one portal PNG. The full image is
// what the model sees; the thumbnail is what the question bubble draws.
func prepareScreenshot(raw []byte) ([]byte, string, int, int, string, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", 0, 0, "", fmt.Errorf("decode config: %w", err)
	}
	// A small compressed PNG can decode to enormous memory. Bound the pixels
	// before the full decode.
	if int64(cfg.Width)*int64(cfg.Height) > 50_000_000 {
		return nil, "", 0, 0, "", fmt.Errorf("screenshot is %dx%d", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", 0, 0, "", fmt.Errorf("decode: %w", err)
	}
	full := fitImage(img, screenshotMaxEdge)
	var buf bytes.Buffer
	if err := png.Encode(&buf, full); err != nil {
		return nil, "", 0, 0, "", fmt.Errorf("encode png: %w", err)
	}
	mediaType := "image/png"
	if buf.Len() > screenshotPNGMaxSize {
		buf.Reset()
		if err := jpeg.Encode(&buf, full, &jpeg.Options{Quality: 84}); err != nil {
			return nil, "", 0, 0, "", fmt.Errorf("encode jpeg: %w", err)
		}
		mediaType = "image/jpeg"
	}
	var thumbBuf bytes.Buffer
	if err := jpeg.Encode(&thumbBuf, fitImage(full, screenshotThumbEdge), &jpeg.Options{Quality: 82}); err != nil {
		return nil, "", 0, 0, "", fmt.Errorf("encode thumb: %w", err)
	}
	thumb := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(thumbBuf.Bytes())
	return buf.Bytes(), mediaType, cfg.Width, cfg.Height, thumb, nil
}

// fitImage scales img down so neither edge exceeds max. It returns img itself
// when it already fits, and uses a high-quality filter because screenshot text
// is the point.
func fitImage(img image.Image, max int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return img
	}
	scale := float64(max) / float64(w)
	if h > w {
		scale = float64(max) / float64(h)
	}
	nw := int(float64(w)*scale + 0.5)
	nh := int(float64(h)*scale + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	return dst
}
