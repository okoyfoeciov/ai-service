//go:build linux

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	portalDestination = "org.freedesktop.portal.Desktop"
	portalObjectPath  = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	portalScreenshot  = "org.freedesktop.portal.Screenshot"
	portalRequest     = "org.freedesktop.portal.Request"
	portalTimeout     = 2 * time.Minute
)

// capturePortalURI asks the desktop portal for one interactive screenshot. It
// opens GNOME's own selection UI, lets the reader drag an area, and returns the
// file URI. It is a variable so a test can replace the D-Bus hop with a local
// file; the rest of captureRegion (read, size check, delete) is real.
var capturePortalURI = func(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, portalTimeout)
	defer cancel()

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return "", fmt.Errorf("session bus: %w", err)
	}
	defer conn.Close()

	token, err := newPortalToken()
	if err != nil {
		return "", fmt.Errorf("portal token: %w", err)
	}
	sender := strings.TrimPrefix(conn.Names()[0], ":")
	sender = strings.ReplaceAll(sender, ".", "_")
	handle := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + token)

	// Match only the portal's own Response signals. The well-known name is
	// resolved to its unique owner because D-Bus matches the sender header.
	var owner string
	if err := conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, portalDestination).Store(&owner); err != nil {
		return "", fmt.Errorf("portal owner: %w", err)
	}
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)
	match := []dbus.MatchOption{
		dbus.WithMatchSender(owner),
		dbus.WithMatchInterface(portalRequest),
		dbus.WithMatchMember("Response"),
	}
	if err := conn.AddMatchSignal(match...); err != nil {
		return "", fmt.Errorf("watch portal response: %w", err)
	}
	defer conn.RemoveMatchSignal(match...)

	obj := conn.Object(portalDestination, portalObjectPath)
	options := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(token),
		"interactive":  dbus.MakeVariant(true),
		"modal":        dbus.MakeVariant(true),
	}
	call := obj.CallWithContext(ctx, portalScreenshot+".Screenshot", 0, "", options)
	if call.Err != nil {
		return "", fmt.Errorf("screenshot portal: %w", call.Err)
	}
	var returned dbus.ObjectPath
	if err := call.Store(&returned); err != nil {
		return "", fmt.Errorf("screenshot portal handle: %w", err)
	}
	if returned != "" {
		handle = returned
	}

	for {
		select {
		case sig, ok := <-signals:
			if !ok {
				return "", fmt.Errorf("session bus closed")
			}
			uri, code, ours, err := portalResponse(sig, handle)
			if !ours {
				continue
			}
			if err != nil {
				return "", err
			}
			switch code {
			case 0:
				return uri, nil
			case 1:
				return "", errScreenshotCancelled
			default:
				return "", fmt.Errorf("portal response code %d", code)
			}
		case <-conn.Context().Done():
			return "", fmt.Errorf("session bus closed")
		case <-ctx.Done():
			// The response may already be queued when the client disappears.
			// Drain it once so a PNG that was saved is still read and deleted.
			if uri, ok := drainPortalURI(signals, handle); ok {
				return uri, nil
			}
			// Closing the request dismisses the portal on backends that
			// support it; GNOME's interactive shell dialog may outlive it.
			cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
			req := conn.Object(portalDestination, handle)
			_ = req.CallWithContext(cctx, portalRequest+".Close", 0).Err
			ccancel()
			return "", ctx.Err()
		}
	}
}

// portalResponse parses one Request.Response signal. ours is false for a
// signal about a different request.
func portalResponse(sig *dbus.Signal, handle dbus.ObjectPath) (uri string, code uint32, ours bool, err error) {
	if sig == nil || sig.Path != handle || sig.Name != portalRequest+".Response" {
		return "", 0, false, nil
	}
	if len(sig.Body) < 2 {
		return "", 0, true, fmt.Errorf("portal response has %d fields", len(sig.Body))
	}
	code, ok := sig.Body[0].(uint32)
	if !ok {
		return "", 0, true, fmt.Errorf("portal response code is %T", sig.Body[0])
	}
	if code != 0 {
		return "", code, true, nil
	}
	results, ok := sig.Body[1].(map[string]dbus.Variant)
	if !ok {
		return "", code, true, fmt.Errorf("portal response body is %T", sig.Body[1])
	}
	uriVariant, ok := results["uri"]
	if !ok {
		return "", code, true, fmt.Errorf("portal response has no uri")
	}
	uri, ok = uriVariant.Value().(string)
	if !ok || uri == "" {
		return "", code, true, fmt.Errorf("portal uri is %T", uriVariant.Value())
	}
	return uri, code, true, nil
}

// drainPortalURI returns a successful response that was already queued when
// the request context ended. It never blocks.
func drainPortalURI(signals <-chan *dbus.Signal, handle dbus.ObjectPath) (string, bool) {
	for {
		select {
		case sig, ok := <-signals:
			if !ok {
				return "", false
			}
			uri, code, ours, err := portalResponse(sig, handle)
			if ours && err == nil && code == 0 {
				return uri, true
			}
		default:
			return "", false
		}
	}
}

// captureRegion runs the portal and turns its file into bytes. The file is
// read and removed immediately: the image is the Ask context, not something
// that should accumulate in Pictures.
func captureRegion(ctx context.Context) ([]byte, error) {
	uri, err := capturePortalURI(ctx)
	if err != nil {
		return nil, err
	}
	return readScreenshotURI(uri)
}

func readScreenshotURI(uri string) ([]byte, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("screenshot uri: %w", err)
	}
	if u.Scheme != "file" || u.Path == "" {
		return nil, fmt.Errorf("screenshot uri scheme %q", u.Scheme)
	}
	return readScreenshotFile(u.Path)
}

func newPortalToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "comart_" + hex.EncodeToString(b[:]), nil
}
