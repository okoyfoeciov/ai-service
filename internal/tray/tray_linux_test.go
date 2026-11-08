//go:build linux

package tray

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"ai-service/internal/config"
)

func TestLinuxMenuHasAllSections(t *testing.T) {
	c := &fakeController{
		transcribe: []string{config.ProviderAssemblyAI, config.ProviderDeepgram},
		explain:    []string{config.ProviderClaude, config.ProviderOpenRouter},
		ask:        []string{config.ProviderClaude, config.ProviderCerebras},
		currentT:   config.ProviderAssemblyAI,
		currentE:   config.ProviderClaude,
		currentA:   config.ProviderClaude,
	}
	var picked []string
	m := newMenu(c, func(kind, provider string) {
		picked = append(picked, kind+"/"+provider)
	})

	root := m.buildLocked()
	if len(root.V2) != 9 {
		t.Fatalf("menu has %d children, want 3 headers and 6 items", len(root.V2))
	}
	wantLabels := []string{
		"Transcribe with", "AssemblyAI Universal-3.5 Pro", "Deepgram Nova-3",
		"Explain with", "Claude Platform Opus 5.5", "OpenRouter Gemini 3.8 Flash",
		"Ask with", "Claude Platform Opus 5.5 (search + fetch)", "Cerebras Qwen 3.8 27B",
	}
	for i, want := range wantLabels {
		child := root.V2[i].Value().(layout)
		if got := child.V1["label"].Value().(string); got != want {
			t.Errorf("child %d label %q, want %q", i, got, want)
		}
	}

	// id 5 is explain Claude (already current), id 6 is explain OpenRouter,
	// id 3 is Deepgram, id 8 is ask Claude (already current), id 9 is ask
	// Cerebras.
	m.pick(5)
	if len(picked) != 0 {
		t.Fatalf("picking the current provider called back: %v", picked)
	}
	m.pick(6)
	if c.currentE != config.ProviderOpenRouter {
		t.Fatalf("explain provider %q, want %q", c.currentE, config.ProviderOpenRouter)
	}
	m.pick(8)
	if len(picked) != 1 {
		t.Fatalf("picking the current ask provider called back: %v", picked)
	}
	m.pick(9)
	if c.currentA != config.ProviderCerebras {
		t.Fatalf("ask provider %q, want %q", c.currentA, config.ProviderCerebras)
	}
	m.pick(3)
	if c.currentT != config.ProviderDeepgram {
		t.Fatalf("transcribe provider %q, want %q", c.currentT, config.ProviderDeepgram)
	}
	if len(picked) != 3 || picked[0] != PickExplain+"/"+config.ProviderOpenRouter ||
		picked[1] != PickAsk+"/"+config.ProviderCerebras ||
		picked[2] != PickTranscribe+"/"+config.ProviderDeepgram {
		t.Fatalf("picks %v", picked)
	}
}

// testBusConfig is a session bus with no service directories, so nothing on
// the machine can be activated on it.
const testBusConfig = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>%s</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`

// startTestBus runs a private dbus-daemon and returns once it listens. The
// returned stop kills it, as when GNOME stops dbus.service at logout.
func startTestBus(t *testing.T, busConfig string) (stop func()) {
	t.Helper()
	cmd := exec.Command("dbus-daemon", "--config-file="+busConfig, "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dbus-daemon: %v", err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(stop)
	// dbus-daemon prints its address once the socket is listening.
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatalf("dbus-daemon did not start: %v", err)
	}
	return stop
}

type fakeWatcher struct{ registered chan string }

func (w *fakeWatcher) RegisterStatusNotifierItem(sender dbus.Sender, path string) *dbus.Error {
	if path == sniPath {
		select {
		case w.registered <- string(sender):
		default:
		}
	}
	return nil
}

// testWatcher owns org.kde.StatusNotifierWatcher on a test bus. registered
// receives the unique name of each item that registers with it.
type testWatcher struct {
	conn       *dbus.Conn
	registered chan string
}

func startWatcher(t *testing.T, addr string) *testWatcher {
	t.Helper()
	conn := dialTestBus(t, addr)
	fake := &fakeWatcher{registered: make(chan string, 10)}
	if err := conn.Export(fake, watcherPath, watcherName); err != nil {
		t.Fatal(err)
	}
	w := &testWatcher{conn: conn, registered: fake.registered}
	w.own(t)
	return w
}

func (w *testWatcher) own(t *testing.T) {
	t.Helper()
	if reply, err := w.conn.RequestName(watcherName, dbus.NameFlagDoNotQueue); err != nil ||
		reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("own %s: %v %v", watcherName, reply, err)
	}
}

func (w *testWatcher) waitRegistered(t *testing.T) string {
	t.Helper()
	select {
	case item := <-w.registered:
		return item
	case <-time.After(15 * time.Second):
		t.Fatal("the item never registered with the watcher")
		return ""
	}
}

// noRegistration fails if the item registered again with nothing prompting it:
// one bus, one registration.
func (w *testWatcher) noRegistration(t *testing.T) {
	t.Helper()
	select {
	case item := <-w.registered:
		t.Fatalf("%s registered twice with the same watcher", item)
	default:
	}
}

// restart drops the watcher name and takes it back, as when GNOME Shell or its
// AppIndicator extension restarts, until the item registers again. Repeating
// covers an owner change that lands before the item has added its match rule.
func (w *testWatcher) restart(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := w.conn.ReleaseName(watcherName); err != nil {
			t.Fatalf("release %s: %v", watcherName, err)
		}
		w.own(t)
		select {
		case item := <-w.registered:
			return item
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the item did not register again after the watcher restarted")
		}
	}
}

func dialTestBus(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// syncBuffer collects log output written from the tray's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitLog(t *testing.T, logs *syncBuffer, line string, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(logs.String(), line) < count {
		if time.Now().After(deadline) {
			t.Fatalf("log never had %q %d times:\n%s", line, count, logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkItem asserts that the item's properties and menu are served on conn,
// and returns the menu's toggle-state by label.
func checkItem(t *testing.T, conn *dbus.Conn, item string, wantVersion uint32) map[string]int32 {
	t.Helper()
	sni := conn.Object(item, sniPath)
	for _, name := range []string{"Title", "Id"} {
		v, err := sni.GetProperty(sniIface + "." + name)
		if err != nil || v.Value() != "ai-service" {
			t.Fatalf("%s = %v, %v; want ai-service", name, v, err)
		}
	}
	menuObj := conn.Object(item, menuPath)
	if v, err := menuObj.GetProperty(menuIface + ".Version"); err != nil || v.Value() != wantVersion {
		t.Fatalf("menu Version = %v, %v; want %d", v, err, wantVersion)
	}
	var revision uint32
	var root layout
	if err := menuObj.Call(menuIface+".GetLayout", 0, int32(0), int32(-1), []string{}).
		Store(&revision, &root); err != nil {
		t.Fatalf("GetLayout: %v", err)
	}
	if revision != wantVersion {
		t.Fatalf("GetLayout revision %d, want %d", revision, wantVersion)
	}
	states := map[string]int32{}
	for _, v := range root.V2 {
		var child layout
		if err := dbus.Store([]interface{}{v.Value()}, &child); err != nil {
			t.Fatalf("menu child %v: %v", v, err)
		}
		label, _ := child.V1["label"].Value().(string)
		state, _ := child.V1["toggle-state"].Value().(int32)
		states[label] = state
	}
	return states
}

func click(t *testing.T, conn *dbus.Conn, item string, id int32) {
	t.Helper()
	if err := conn.Object(item, menuPath).Call(menuIface+".Event", 0,
		id, "clicked", dbus.MakeVariant(""), uint32(0)).Err; err != nil {
		t.Fatalf("click %d: %v", id, err)
	}
}

func waitPick(t *testing.T, picked <-chan string, want string) {
	t.Helper()
	select {
	case got := <-picked:
		if got != want {
			t.Fatalf("picked %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no pick of %q", want)
	}
}

const registerFunc = "ai-service/internal/tray.register"

// goroutinesIn counts the goroutines whose stack includes the function fn.
func goroutinesIn(fn string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "\n"+fn+"(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

func waitGoroutines(t *testing.T, fn string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := goroutinesIn(fn)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines in %s, want %d", got, fn, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// GNOME stops the user's dbus.service at logout and a new one starts at the
// next login, while the lingering service keeps running. The tray must come
// back on the new bus with the same picks.
func TestLinuxTrayReconnectsAfterBusRestart(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon not on PATH")
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "bus")
	if len(socket) >= 100 {
		t.Skipf("socket path too long for AF_UNIX: %s", socket)
	}
	addr := "unix:path=" + socket
	busConfig := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(busConfig, []byte(fmt.Sprintf(testBusConfig, addr)), 0o600); err != nil {
		t.Fatal(err)
	}
	// run gets addr directly and never reads the environment. The variable
	// points at the test bus as well, so that a regression to
	// dbus.SessionBus() or ConnectSessionBus cannot reach the desktop's bus.
	// Registered first, so it is restored last, after the tray has stopped.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", addr)
	logs := &syncBuffer{}
	prevLog := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prevLog) })

	stopBus := startTestBus(t, busConfig)
	watcher := startWatcher(t, addr)

	c := &fakeController{
		transcribe: []string{config.ProviderAssemblyAI, config.ProviderDeepgram},
		explain:    []string{config.ProviderClaude, config.ProviderOpenRouter},
		ask:        []string{config.ProviderClaude},
		currentT:   config.ProviderAssemblyAI,
		currentE:   config.ProviderClaude,
		currentA:   config.ProviderClaude,
	}
	picked := make(chan string, 4)
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(c, func(kind, provider string) { picked <- kind + "/" + provider }, quit, addr)
	}()
	var quitOnce sync.Once
	stopTray := func() {
		quitOnce.Do(func() { close(quit) })
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("run did not return within 2s of quit:\n%s", logs.String())
			<-done // never let the tray outlive the test's environment
		}
	}
	t.Cleanup(stopTray)

	// First bus: register, then pick Deepgram (id 3: transcribe header 1,
	// AssemblyAI 2, Deepgram 3).
	item := watcher.waitRegistered(t)
	client := dialTestBus(t, addr)
	if states := checkItem(t, client, item, 1); states["AssemblyAI Universal-3.5 Pro"] != 1 {
		t.Fatalf("initial menu states %v", states)
	}
	click(t, client, item, 3)
	waitPick(t, picked, PickTranscribe+"/"+config.ProviderDeepgram)
	watcher.noRegistration(t)

	// Logout: the bus goes away, and a new one comes up on the same address
	// with no watcher until the next login, so the item waits for one.
	stopBus()
	waitLog(t, logs, "tray: session bus lost; reconnecting", 1)
	stopBus = startTestBus(t, busConfig)
	waitLog(t, logs, "tray: reconnected", 1)
	waitGoroutines(t, registerFunc, 1)

	// That bus goes away too while the item is still waiting. The waiting
	// register goroutine must end with its connection (checked at the end).
	stopBus()
	waitLog(t, logs, "tray: session bus lost; reconnecting", 2)
	stopBus = startTestBus(t, busConfig)
	waitLog(t, logs, "tray: reconnected", 2)

	// Login: a watcher appears and the item registers with it.
	watcher = startWatcher(t, addr)
	item = watcher.waitRegistered(t)
	client = dialTestBus(t, addr)
	states := checkItem(t, client, item, 2)
	if states["AssemblyAI Universal-3.5 Pro"] != 0 || states["Deepgram Nova-3"] != 1 ||
		states["Claude Platform Opus 5.5"] != 1 {
		t.Fatalf("menu states after reconnect %v, want Deepgram and Claude checked", states)
	}

	// A pick on the new bus announces the new Version on the new connection.
	if err := client.AddMatchSignal(
		dbus.WithMatchSender(item),
		dbus.WithMatchObjectPath(menuPath),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		t.Fatal(err)
	}
	signals := make(chan *dbus.Signal, 10)
	client.Signal(signals)
	click(t, client, item, 2)
	waitPick(t, picked, PickTranscribe+"/"+config.ProviderAssemblyAI)
	select {
	case sig := <-signals:
		if sig == nil || len(sig.Body) < 2 {
			t.Fatalf("PropertiesChanged %v", sig)
		}
		changed, _ := sig.Body[1].(map[string]dbus.Variant)
		if v, ok := changed["Version"]; !ok || v.Value() != uint32(3) {
			t.Fatalf("PropertiesChanged %v, want Version 3", sig.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no PropertiesChanged for the menu Version on the new bus")
	}
	watcher.noRegistration(t)

	// The register goroutine on the new connection still follows the watcher.
	if again := watcher.restart(t); again != item {
		t.Fatalf("re-registered as %s, want %s", again, item)
	}

	// Quit still ends Run promptly while it is waiting to reconnect, and no
	// register goroutine outlives its connection.
	stopBus()
	waitLog(t, logs, "tray: session bus lost; reconnecting", 3)
	stopTray()
	waitGoroutines(t, registerFunc, 0)
}
