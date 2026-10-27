//go:build linux

package tray

import (
	"bytes"
	"image"
	_ "image/png"
	"log"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// Linux deliberately does not use fyne.io/systray's Linux backend. That
// backend exports an Activate method, and GNOME's AppIndicator extension waits
// out the double-click interval before opening a menu for any item that
// advertises Activate. Other tray icons omit it and open on the first click.
// This implementation is the same StatusNotifierItem/dbusmenu protocol without
// Activate, so a single click opens the picker immediately.
const (
	sniPath     = "/StatusNotifierItem"
	menuPath    = "/StatusNotifierItem/menu"
	sniIface    = "org.kde.StatusNotifierItem"
	menuIface   = "com.canonical.dbusmenu"
	watcherName = "org.kde.StatusNotifierWatcher"
	watcherPath = "/StatusNotifierWatcher"
)

var (
	quitOnce sync.Once
	quitCh   = make(chan struct{})
)

// pixmap is the SNI image type: width, height, then ARGB bytes.
type pixmap struct {
	W, H int
	Pix  []byte
}

// tooltip is the SNI ToolTip property, signature (sa(iiay)ss).
type tooltip struct {
	V0 string
	V1 []pixmap
	V2 string
	V3 string
}

type statusNotifier struct {
	icon pixmap
}

// Run exports a StatusNotifierItem and its dbusmenu, then blocks until Quit.
func Run(c Controller, onPick func(string)) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		log.Printf("tray: no session D-Bus available — provider picker needs a desktop session")
		return
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		log.Printf("tray: session bus: %v", err)
		return
	}
	defer conn.Close()

	sni := &statusNotifier{icon: decodePixmap(iconRegular)}
	if _, err := prop.Export(conn, sniPath, sni.spec()); err != nil {
		log.Printf("tray: export status item: %v", err)
		return
	}

	m := &menu{
		providers:  c.TranscribeProviders(),
		current:    c.TranscribeProvider(),
		controller: c,
		onPick:     onPick,
		revision:   1,
	}
	menuProps, err := prop.Export(conn, menuPath, m.spec())
	if err != nil {
		log.Printf("tray: export menu properties: %v", err)
		return
	}
	m.props = menuProps
	if err := conn.ExportSubtreeMethodTable(map[string]interface{}{
		"GetLayout":          m.GetLayout,
		"GetGroupProperties": m.GetGroupProperties,
		"GetProperty":        m.GetProperty,
		"Event":              m.Event,
		"EventGroup":         m.EventGroup,
		"AboutToShow":        m.AboutToShow,
		"AboutToShowGroup":   m.AboutToShowGroup,
	}, menuPath, menuIface); err != nil {
		log.Printf("tray: export menu methods: %v", err)
		return
	}

	sniNode := introspect.Node{
		Name:       sniPath,
		Interfaces: []introspect.Interface{introspect.IntrospectData, prop.IntrospectData, sniInterface},
	}
	if err := conn.Export(introspect.NewIntrospectable(&sniNode), sniPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		log.Printf("tray: export status introspection: %v", err)
		return
	}
	menuNode := introspect.Node{
		Name:       menuPath,
		Interfaces: []introspect.Interface{introspect.IntrospectData, prop.IntrospectData, menuInterface},
	}
	if err := conn.Export(introspect.NewIntrospectable(&menuNode), menuPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		log.Printf("tray: export menu introspection: %v", err)
		return
	}

	// Register by object path only. Requesting a well-known
	// org.kde.StatusNotifierItem-<pid> name as well makes the GNOME AppIndicator
	// watcher discover the same item twice (once by that name, once by the path
	// registration below), which shows two identical tray icons after a shell
	// restart.
	go register(conn)
	log.Printf("tray: provider picker ready")

	<-quitCh
}

// Quit ends Run.
func Quit() {
	quitOnce.Do(func() { close(quitCh) })
}

func (s *statusNotifier) spec() map[string]map[string]*prop.Prop {
	return map[string]map[string]*prop.Prop{
		sniIface: {
			"Category":            {Value: "ApplicationStatus", Emit: prop.EmitTrue},
			"Id":                  {Value: "ai-service", Emit: prop.EmitTrue},
			"Title":               {Value: "ai-service", Emit: prop.EmitTrue},
			"Status":              {Value: "Active", Emit: prop.EmitTrue},
			"WindowId":            {Value: int32(0), Emit: prop.EmitTrue},
			"IconName":            {Value: "", Emit: prop.EmitTrue},
			"IconPixmap":          {Value: []pixmap{s.icon}, Writable: true, Emit: prop.EmitTrue},
			"OverlayIconName":     {Value: "", Emit: prop.EmitTrue},
			"OverlayIconPixmap":   {Value: []pixmap{}, Emit: prop.EmitTrue},
			"AttentionIconName":   {Value: "", Emit: prop.EmitTrue},
			"AttentionIconPixmap": {Value: []pixmap{}, Emit: prop.EmitTrue},
			"AttentionMovieName":  {Value: "", Emit: prop.EmitTrue},
			"ToolTip":             {Value: tooltip{V2: "ai-service — transcription provider"}, Emit: prop.EmitTrue},
			"ItemIsMenu":          {Value: true, Emit: prop.EmitTrue},
			"Menu":                {Value: dbus.ObjectPath(menuPath), Writable: true, Emit: prop.EmitTrue},
			"IconThemePath":       {Value: "", Emit: prop.EmitTrue},
		},
	}
}

var sniInterface = introspect.Interface{
	Name: sniIface,
	Properties: []introspect.Property{
		{Name: "Category", Type: "s", Access: "read"},
		{Name: "Id", Type: "s", Access: "read"},
		{Name: "Title", Type: "s", Access: "read"},
		{Name: "Status", Type: "s", Access: "read"},
		{Name: "WindowId", Type: "i", Access: "read"},
		{Name: "IconName", Type: "s", Access: "read"},
		{Name: "IconPixmap", Type: "a(iiay)", Access: "read"},
		{Name: "OverlayIconName", Type: "s", Access: "read"},
		{Name: "OverlayIconPixmap", Type: "a(iiay)", Access: "read"},
		{Name: "AttentionIconName", Type: "s", Access: "read"},
		{Name: "AttentionIconPixmap", Type: "a(iiay)", Access: "read"},
		{Name: "AttentionMovieName", Type: "s", Access: "read"},
		{Name: "ToolTip", Type: "(sa(iiay)ss)", Access: "read"},
		{Name: "ItemIsMenu", Type: "b", Access: "read"},
		{Name: "Menu", Type: "o", Access: "read"},
		{Name: "IconThemePath", Type: "s", Access: "read"},
	},
	Signals: []introspect.Signal{
		{Name: "NewIcon"},
		{Name: "NewTitle"},
		{Name: "NewToolTip"},
		{Name: "NewStatus"},
	},
}

// layout is the dbusmenu tree node: id, properties, children.
type layout struct {
	V0 int32
	V1 map[string]dbus.Variant
	V2 []dbus.Variant
}

type menu struct {
	mu         sync.Mutex
	providers  []string
	current    string
	controller Controller
	onPick     func(string)
	revision   uint32
	props      *prop.Properties
}

func (m *menu) spec() map[string]map[string]*prop.Prop {
	return map[string]map[string]*prop.Prop{
		menuIface: {
			"Version":       {Value: m.revision, Writable: true, Emit: prop.EmitTrue},
			"TextDirection": {Value: "ltr", Emit: prop.EmitTrue},
			"Status":        {Value: "normal", Emit: prop.EmitTrue},
			"IconThemePath": {Value: []string{}, Emit: prop.EmitTrue},
		},
	}
}

var menuInterface = introspect.Interface{
	Name: menuIface,
	Methods: []introspect.Method{
		{Name: "GetLayout", Args: []introspect.Arg{
			{Name: "parentId", Type: "i", Direction: "in"},
			{Name: "recursionDepth", Type: "i", Direction: "in"},
			{Name: "propertyNames", Type: "as", Direction: "in"},
			{Name: "revision", Type: "u", Direction: "out"},
			{Name: "layout", Type: "(ia{sv}av)", Direction: "out"},
		}},
		{Name: "GetGroupProperties", Args: []introspect.Arg{
			{Name: "ids", Type: "ai", Direction: "in"},
			{Name: "propertyNames", Type: "as", Direction: "in"},
			{Name: "properties", Type: "a(ia{sv})", Direction: "out"},
		}},
		{Name: "GetProperty", Args: []introspect.Arg{
			{Name: "id", Type: "i", Direction: "in"},
			{Name: "name", Type: "s", Direction: "in"},
			{Name: "value", Type: "v", Direction: "out"},
		}},
		{Name: "Event", Args: []introspect.Arg{
			{Name: "id", Type: "i", Direction: "in"},
			{Name: "eventId", Type: "s", Direction: "in"},
			{Name: "data", Type: "v", Direction: "in"},
			{Name: "timestamp", Type: "u", Direction: "in"},
		}},
		{Name: "EventGroup", Args: []introspect.Arg{
			{Name: "events", Type: "a(isvu)", Direction: "in"},
			{Name: "idErrors", Type: "ai", Direction: "out"},
		}},
		{Name: "AboutToShow", Args: []introspect.Arg{
			{Name: "id", Type: "i", Direction: "in"},
			{Name: "needUpdate", Type: "b", Direction: "out"},
		}},
		{Name: "AboutToShowGroup", Args: []introspect.Arg{
			{Name: "ids", Type: "ai", Direction: "in"},
			{Name: "updatesNeeded", Type: "ai", Direction: "out"},
			{Name: "idErrors", Type: "ai", Direction: "out"},
		}},
	},
	Properties: []introspect.Property{
		{Name: "Version", Type: "u", Access: "read"},
		{Name: "TextDirection", Type: "s", Access: "read"},
		{Name: "Status", Type: "s", Access: "read"},
		{Name: "IconThemePath", Type: "as", Access: "read"},
	},
	Signals: []introspect.Signal{
		{Name: "ItemsPropertiesUpdated", Args: []introspect.Arg{
			{Name: "updatedProps", Type: "a(ia{sv})", Direction: "out"},
			{Name: "removedProps", Type: "a(ias)", Direction: "out"},
		}},
		{Name: "LayoutUpdated", Args: []introspect.Arg{
			{Name: "revision", Type: "u", Direction: "out"},
			{Name: "parent", Type: "i", Direction: "out"},
		}},
	},
}

func (m *menu) buildLocked() layout {
	root := layout{V0: 0, V1: map[string]dbus.Variant{}, V2: []dbus.Variant{}}
	header := layout{
		V0: 1,
		V1: map[string]dbus.Variant{
			"label":   dbus.MakeVariant("Transcribe with"),
			"enabled": dbus.MakeVariant(false),
			"visible": dbus.MakeVariant(true),
			"type":    dbus.MakeVariant("standard"),
		},
		V2: []dbus.Variant{},
	}
	root.V2 = append(root.V2, dbus.MakeVariant(header))
	for i, p := range m.providers {
		state := int32(0)
		if p == m.current {
			state = 1
		}
		item := layout{
			V0: int32(i + 2),
			V1: map[string]dbus.Variant{
				"label":        dbus.MakeVariant(providerLabel(p)),
				"enabled":      dbus.MakeVariant(true),
				"visible":      dbus.MakeVariant(true),
				"type":         dbus.MakeVariant("standard"),
				"toggle-type":  dbus.MakeVariant("checkmark"),
				"toggle-state": dbus.MakeVariant(state),
			},
			V2: []dbus.Variant{},
		}
		root.V2 = append(root.V2, dbus.MakeVariant(item))
	}
	return root
}

func (m *menu) findLocked(id int32) (layout, bool) {
	root := m.buildLocked()
	if id == 0 {
		return root, true
	}
	for _, v := range root.V2 {
		child := v.Value().(layout)
		if child.V0 == id {
			return child, true
		}
	}
	return layout{}, false
}

func (m *menu) GetLayout(parentID int32, recursionDepth int32, propertyNames []string) (uint32, layout, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	node, ok := m.findLocked(parentID)
	if !ok {
		return m.revision, layout{V0: parentID, V1: map[string]dbus.Variant{}, V2: []dbus.Variant{}}, nil
	}
	if recursionDepth == 0 {
		node.V2 = []dbus.Variant{}
	}
	return m.revision, node, nil
}

func (m *menu) GetGroupProperties(ids []int32, propertyNames []string) ([]struct {
	V0 int32
	V1 map[string]dbus.Variant
}, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]struct {
		V0 int32
		V1 map[string]dbus.Variant
	}, 0, len(ids))
	for _, id := range ids {
		if node, ok := m.findLocked(id); ok {
			out = append(out, struct {
				V0 int32
				V1 map[string]dbus.Variant
			}{id, node.V1})
		}
	}
	return out, nil
}

func (m *menu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if node, ok := m.findLocked(id); ok {
		if v, exists := node.V1[name]; exists {
			return v, nil
		}
	}
	return dbus.MakeVariant(""), nil
}

func (m *menu) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventID == "clicked" {
		m.pick(id)
	}
	return nil
}

func (m *menu) EventGroup(events []struct {
	V0 int32
	V1 string
	V2 dbus.Variant
	V3 uint32
}) ([]int32, *dbus.Error) {
	for _, e := range events {
		if e.V1 == "clicked" {
			m.pick(e.V0)
		}
	}
	return nil, nil
}

func (m *menu) AboutToShow(id int32) (bool, *dbus.Error) {
	// Returning true makes the shell re-read GetLayout before opening, so the
	// checkmarks always reflect the provider picked last.
	return true, nil
}

func (m *menu) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return ids, nil, nil
}

func (m *menu) pick(id int32) {
	idx := int(id - 2)
	m.mu.Lock()
	if id < 2 || idx >= len(m.providers) {
		m.mu.Unlock()
		return
	}
	p := m.providers[idx]
	if p == m.current || !m.controller.SetTranscribeProvider(p) {
		m.mu.Unlock()
		return
	}
	m.current = p
	m.revision++
	revision, props := m.revision, m.props
	m.mu.Unlock()

	if props != nil {
		if err := props.Set(menuIface, "Version", dbus.MakeVariant(revision)); err != nil {
			log.Printf("tray: update menu version: %v", err)
		}
	}
	log.Printf("transcribe provider: %s (tray)", providerLabel(p))
	if m.onPick != nil {
		m.onPick(p)
	}
}

func register(conn *dbus.Conn) {
	watcher := conn.Object(watcherName, watcherPath)
	registerOnce := func() bool {
		return watcher.Call(watcherName+".RegisterStatusNotifierItem", 0, sniPath).Err == nil
	}
	for !registerOnce() {
		select {
		case <-quitCh:
			return
		case <-time.After(time.Second):
		}
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, watcherName),
	); err != nil {
		log.Printf("tray: watch %s: %v", watcherName, err)
		return
	}
	ch := make(chan *dbus.Signal, 10)
	conn.Signal(ch)
	for {
		select {
		case <-quitCh:
			return
		case sig := <-ch:
			if sig == nil {
				return
			}
			if len(sig.Body) >= 3 {
				if newOwner, ok := sig.Body[2].(string); ok && newOwner != "" {
					registerOnce()
				}
			}
		}
	}
}

func decodePixmap(data []byte) pixmap {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		log.Printf("tray: decode icon: %v", err)
		return pixmap{}
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, 0, w*h*4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			out = append(out, byte(a>>8), byte(r>>8), byte(g>>8), byte(bl>>8))
		}
	}
	return pixmap{W: w, H: h, Pix: out}
}
