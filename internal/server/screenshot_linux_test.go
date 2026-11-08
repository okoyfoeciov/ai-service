//go:build linux

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakePortalFile writes a real PNG to a temp file and returns its file URI.
func fakePortalFile(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 90, 255})
		}
	}
	path := filepath.Join(t.TempDir(), "screenshot.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return "file://" + path
}

func withFakePortal(t *testing.T, uri string) {
	t.Helper()
	old := capturePortalURI
	capturePortalURI = func(context.Context) (string, error) { return uri, nil }
	t.Cleanup(func() { capturePortalURI = old })
}

func TestCaptureRegionReadsAndDeletesThePortalFile(t *testing.T) {
	uri := fakePortalFile(t, 640, 480)
	path := strings.TrimPrefix(uri, "file://")
	withFakePortal(t, uri)
	raw, err := captureRegion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("empty screenshot")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("portal file still exists after capture: %v", err)
	}
}

func TestPrepareScreenshotBoundsFullImageAndBuildsThumb(t *testing.T) {
	uri := fakePortalFile(t, 2000, 1000)
	withFakePortal(t, uri)
	raw, err := captureRegion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	full, media, w, h, thumb, err := prepareScreenshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	if w != 2000 || h != 1000 {
		t.Errorf("original size %dx%d", w, h)
	}
	if media != "image/png" && media != "image/jpeg" {
		t.Errorf("media %q", media)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width > screenshotMaxEdge || cfg.Height > screenshotMaxEdge {
		t.Errorf("full image %dx%d exceeds %d", cfg.Width, cfg.Height, screenshotMaxEdge)
	}
	if !strings.HasPrefix(thumb, "data:image/jpeg;base64,") {
		t.Errorf("thumb prefix %q", thumb[:min(len(thumb), 40)])
	}
}

func TestScreenshotRouteStoresAndReleases(t *testing.T) {
	uri := fakePortalFile(t, 800, 600)
	withFakePortal(t, uri)
	s := &Server{shots: newScreenshotStore(time.Hour, 8<<20, 8)}
	req := httptest.NewRequest(http.MethodPost, "/api/screenshot", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8770"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	var got struct {
		ID     string `json:"id"`
		Thumb  string `json:"thumb"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID == "" || !strings.HasPrefix(got.Thumb, "data:image/jpeg;base64,") ||
		got.Width != 800 || got.Height != 600 {
		t.Fatalf("response %+v", got)
	}
	imgs, err := s.shots.getMany([]string{got.ID})
	if err != nil || len(imgs) != 1 || imgs[0].MediaType == "" {
		t.Fatalf("stored image: %v %+v", err, imgs)
	}
	body := fmt.Sprintf(`{"ids":[%q]}`, got.ID)
	rel := httptest.NewRequest(http.MethodPost, "/api/screenshot/release", strings.NewReader(body))
	rel.Host = "127.0.0.1:8770"
	rel.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, rel)
	if rec.Code != http.StatusOK {
		t.Fatalf("release status %d body %q", rec.Code, rec.Body.String())
	}
	if _, err := s.shots.getMany([]string{got.ID}); err == nil {
		t.Fatal("released screenshot is still in the store")
	}
}

// fakePortal is a private session bus's org.freedesktop.portal.Desktop. It
// answers Screenshot with the request's predicted handle and then emits the
// Response signal with a file URI, exactly as the real portal does after the
// reader selects an area. The sender argument is injected by godbus.
type fakePortal struct {
	conn *dbus.Conn
	uri  string
}

func (p *fakePortal) Screenshot(sender dbus.Sender, parent string, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	token, _ := options["handle_token"].Value().(string)
	base := strings.TrimPrefix(string(sender), ":")
	base = strings.ReplaceAll(base, ".", "_")
	handle := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + base + "/" + token)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = p.conn.Emit(handle, portalRequest+".Response", uint32(0),
			map[string]dbus.Variant{"uri": dbus.MakeVariant(p.uri)})
	}()
	return handle, nil
}

func startPrivateSessionBus(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dbus-daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("dbus-daemon did not start: %v", err)
	}
	return strings.TrimSpace(line)
}

func TestCapturePortalReadsResponseSignal(t *testing.T) {
	uri := fakePortalFile(t, 320, 240)
	path := strings.TrimPrefix(uri, "file://")
	addr := startPrivateSessionBus(t)
	old, had := os.LookupEnv("DBUS_SESSION_BUS_ADDRESS")
	if err := os.Setenv("DBUS_SESSION_BUS_ADDRESS", addr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("DBUS_SESSION_BUS_ADDRESS", old)
		} else {
			_ = os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
		}
	})

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	portal := &fakePortal{conn: conn, uri: uri}
	if err := conn.Export(portal, portalObjectPath, portalScreenshot); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.RequestName(portalDestination, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("own %s: %v %v", portalDestination, reply, err)
	}

	raw, err := captureRegion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("empty screenshot from fake portal")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("portal file still exists: %v", err)
	}
}

func TestScreenshotStoreRefusesWhenFullInsteadOfEvicting(t *testing.T) {
	shots := newScreenshotStore(time.Hour, 1024, 1)
	first, err := shots.put([]byte("one"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shots.put([]byte("two"), "image/png"); !errors.Is(err, errScreenshotFull) {
		t.Fatalf("second put error %v, want full", err)
	}
	if imgs, err := shots.getMany([]string{first}); err != nil || len(imgs) != 1 {
		t.Fatalf("first screenshot was evicted: %v %+v", err, imgs)
	}
}
