package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"ai-service/internal/brave"
	"ai-service/internal/config"
	"ai-service/internal/llm"
)

func imageSearchBody() string {
	return `{
		"query":{"original":"map of Uruguay"},
		"results":[
			{"title":"Map of Uruguay","url":"https://www.mapsland.com/uruguay","source":"mapsland.com","thumbnail":{"src":"https://imgs.search.brave.com/a","width":500,"height":400},"properties":{"url":"https://www.mapsland.com/map.jpg","width":1080,"height":880}},
			{"title":"Stock map","url":"https://www.shutterstock.com/map","source":"shutterstock.com","thumbnail":{"src":"https://imgs.search.brave.com/b","width":500,"height":400},"properties":{"url":"https://www.shutterstock.com/map.jpg","width":1500,"height":1200}},
			{"title":"Political map","url":"https://www.onestopmap.com/uruguay","source":"onestopmap.com","thumbnail":{"src":"https://imgs.search.brave.com/c","width":500,"height":400},"properties":{"url":"https://www.onestopmap.com/map.jpg","width":1800,"height":1928}}
		]
	}`
}

func TestAskImageToolsSearchThenShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageSearchBody()))
	}))
	defer srv.Close()

	s := &Server{images: brave.New("k", srv.Client())}
	s.images.BaseURL = srv.URL
	var events []map[string]any
	send := func(_ string, data any) {
		b, _ := json.Marshal(data)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		events = append(events, m)
	}
	exec := s.askImageExecutor(2, send)

	res := exec(context.Background(), "search_images", json.RawMessage(`{"query":"map of Uruguay"}`), false)
	if res.IsError {
		t.Fatalf("search error: %+v", res)
	}
	if !strings.Contains(res.Text, `"id":"img_1"`) || !strings.Contains(res.Text, `"id":"img_2"`) ||
		strings.Contains(res.Text, "Stock map") {
		t.Fatalf("search text %q", res.Text)
	}
	if len(events) != 1 || events[0]["status"] != "searching" || events[0]["round"] != float64(2) {
		t.Fatalf("events after search %v", events)
	}

	res = exec(context.Background(), "show_images", json.RawMessage(`{"image_ids":["img_1","img_99"]}`), false)
	if res.IsError || !strings.Contains(res.Text, "Displayed 1 image") ||
		!strings.Contains(res.Text, "img_99") {
		t.Fatalf("show result %+v", res)
	}
	if len(events) != 2 || events[1]["status"] != "ready" {
		t.Fatalf("events after show %v", events)
	}
	imgs, _ := events[1]["images"].([]any)
	if len(imgs) != 1 {
		t.Fatalf("shown images %v", events[1]["images"])
	}
	img, _ := imgs[0].(map[string]any)
	if img["id"] != "img_1" || img["thumb"] != "https://imgs.search.brave.com/a" ||
		img["page"] != "https://www.mapsland.com/uruguay" {
		t.Fatalf("shown image %v", img)
	}

	// The same ID is not appended twice in one round.
	res = exec(context.Background(), "show_images", json.RawMessage(`{"image_ids":["img_1"]}`), false)
	if !res.IsError || len(events) != 2 {
		t.Fatalf("duplicate show %+v events %v", res, events)
	}
}

func TestAskImageToolsCapAndClaudeImages(t *testing.T) {
	var thumbCalled bool
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".png") {
			thumbCalled = true
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"Map","url":"https://example.com/map","source":"example.com","thumbnail":{"src":"` +
			srv.URL + `/thumb.png","width":500,"height":400},"properties":{"url":"https://example.com/map.jpg","width":1080,"height":880}}
		]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	s := &Server{images: brave.New("k", srv.Client())}
	s.images.BaseURL = srv.URL
	s.images.ThumbHost = u.Hostname()
	var events []map[string]any
	send := func(_ string, data any) {
		b, _ := json.Marshal(data)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		events = append(events, m)
	}
	exec := s.askImageExecutor(0, send)
	res := exec(context.Background(), "search_images", json.RawMessage(`{"query":"map"}`), true)
	if res.IsError || len(res.Images) != 1 || res.Images[0].Label != "img_1: Map" {
		t.Fatalf("claude search %+v", res)
	}
	if !thumbCalled {
		t.Fatal("thumbnail was not downloaded for Claude")
	}

	for i := 1; i < 4; i++ {
		_ = exec(context.Background(), "search_images", json.RawMessage(`{"query":"map"}`), false)
	}
	res = exec(context.Background(), "search_images", json.RawMessage(`{"query":"map"}`), false)
	if !res.IsError || !strings.Contains(res.Text, "limit") {
		t.Fatalf("fifth search %+v", res)
	}
	last := events[len(events)-1]
	if last["status"] != "error" {
		t.Fatalf("last event %v", last)
	}
}

type fakeToolStreamer struct {
	tools []llm.ToolDef
	exec  llm.ToolExecutor
	text  string
}

func (f *fakeToolStreamer) Stream(ctx context.Context, prompt string, onText func(string)) llm.Result {
	return llm.Result{OK: true}
}

func (f *fakeToolStreamer) StreamWithTools(ctx context.Context, prompt string, images []llm.Image, tools []llm.ToolDef, exec llm.ToolExecutor, onText func(string), onSearch func(bool)) llm.Result {
	f.tools = tools
	f.exec = exec
	res := exec(ctx, "search_images", json.RawMessage(`{"query":"map of Uruguay"}`), false)
	if res.IsError {
		return llm.Result{Code: "tool", Message: res.Text}
	}
	_ = exec(ctx, "show_images", json.RawMessage(`{"image_ids":["img_1"]}`), false)
	if f.text != "" {
		onText(f.text)
	}
	return llm.Result{OK: true}
}

func TestHandleAskOffersImageToolsAndStreamsTheGallery(t *testing.T) {
	braveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageSearchBody()))
	}))
	defer braveSrv.Close()

	cfg := config.Config{AskProvider: "fake", AskProviders: []string{"fake"}}
	s := New(cfg, nil)
	s.images = brave.New("k", braveSrv.Client())
	s.images.BaseURL = braveSrv.URL
	fake := &fakeToolStreamer{text: "Here is the map."}
	s.askLLMs = map[string]llm.Streamer{"fake": fake}

	rec := postJSON(t, s, "/api/ask", map[string]any{"question": "show me the map of Uruguay"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(fake.tools) != 2 || fake.tools[0].Name != "search_images" || fake.tools[1].Name != "show_images" {
		t.Fatalf("tools %+v", fake.tools)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: search", "event: images", `"status":"searching"`, `"status":"ready"`,
		`"id":"img_1"`, `"thumb":"https://imgs.search.brave.com/a"`,
		"event: chunk", "Here is the map.", "event: done",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE body lacks %q:\n%s", want, body)
		}
	}
}
