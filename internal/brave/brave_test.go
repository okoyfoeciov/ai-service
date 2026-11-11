package brave

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSearchSendsTheFixedBraveConfiguration(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		if r.URL.Path != "/res/v1/images/search" {
			t.Errorf("path %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Subscription-Token"); got != "secret" {
			t.Errorf("token %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "comart/0.1" {
			t.Errorf("user agent %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query":{"original":"map of Uruguay"},
			"results":[
				{"title":"Map of Uruguay","url":"https://www.mapsland.com/uruguay","source":"mapsland.com","thumbnail":{"src":"https://imgs.search.brave.com/a","width":500,"height":400},"properties":{"url":"https://www.mapsland.com/map.jpg","width":1080,"height":880},"confidence":"high"},
				{"title":"Stock map","url":"https://www.shutterstock.com/map","source":"shutterstock.com","thumbnail":{"src":"https://imgs.search.brave.com/b","width":500,"height":400},"properties":{"url":"https://www.shutterstock.com/map.jpg","width":1500,"height":1200}},
				{"title":"Tiny map","url":"https://example.com/tiny","source":"example.com","thumbnail":{"src":"https://imgs.search.brave.com/c","width":200,"height":100},"properties":{"url":"https://example.com/tiny.jpg","width":200,"height":100}},
				{"title":"Political map","url":"https://www.onestopmap.com/uruguay","source":"onestopmap.com","thumbnail":{"src":"https://imgs.search.brave.com/d","width":500,"height":400},"properties":{"url":"https://www.onestopmap.com/map.jpg","width":1800,"height":1928}}
			]
		}`))
	}))
	defer srv.Close()

	c := New("secret", srv.Client())
	c.BaseURL = srv.URL
	got, err := c.Search(context.Background(), "map of Uruguay")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for key, want := range map[string]string{
		"q": "map of Uruguay", "count": "10", "country": "ALL",
		"search_lang": "en", "safesearch": "strict", "spellcheck": "false",
	} {
		if gotQuery.Get(key) != want {
			t.Errorf("%s = %q, want %q (all %v)", key, gotQuery.Get(key), want, gotQuery)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(got), got)
	}
	if got[0].Title != "Map of Uruguay" || got[0].Source != "mapsland.com" ||
		got[0].Width != 1080 || got[0].Height != 880 {
		t.Errorf("first candidate %+v", got[0])
	}
	if got[1].Title != "Political map" || got[1].Source != "onestopmap.com" {
		t.Errorf("second candidate %+v", got[1])
	}
}

func TestSearchReturnsAnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"detail":"invalid token","code":"UNAUTHORIZED"}}`))
	}))
	defer srv.Close()

	c := New("bad", srv.Client())
	c.BaseURL = srv.URL
	_, err := c.Search(context.Background(), "map")
	if err == nil {
		t.Fatal("Search succeeded, want error")
	}
	be, ok := err.(*Error)
	if !ok || be.Status != http.StatusUnauthorized || !strings.Contains(be.Error(), "invalid token") {
		t.Fatalf("error %T %v", err, err)
	}
}

func TestSearchRejectsAnEmptyKeyAndLongQuery(t *testing.T) {
	c := New("", nil)
	if _, err := c.Search(context.Background(), "map"); err == nil {
		t.Fatal("empty key succeeded")
	}
	c = New("k", nil)
	if _, err := c.Search(context.Background(), strings.Repeat("word ", 60)); err == nil {
		t.Fatal("long query succeeded")
	}
}

func TestDownloadReturnsABase64Image(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "image/*" {
			t.Errorf("accept %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	c := New("k", srv.Client())
	c.ThumbHost = u.Hostname()
	mediaType, data, err := c.Download(context.Background(), srv.URL+"/thumb.png")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if mediaType != "image/png" {
		t.Errorf("media type %q", mediaType)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil || string(raw) != string(png) {
		t.Fatalf("data mismatch: %v", err)
	}
}

func TestSearchResponseShapeIsReadable(t *testing.T) {
	// A small guard against a typo in the JSON tags: marshal a response with
	// the fields the API documents and unmarshal it through the actual type.
	raw := []byte(`{"results":[{"title":"t","url":"https://x.test/p","source":"x.test","thumbnail":{"src":"https://imgs.search.brave.com/t","width":500,"height":400},"properties":{"url":"https://x.test/i.jpg","width":1000,"height":800},"confidence":"high"}]}`)
	var body searchResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 1 || body.Results[0].Properties.Width != 1000 {
		t.Fatalf("shape %+v", body)
	}
}

// net/http owns gzip: it adds the header and decompresses only when the client
// did not set Accept-Encoding by hand. This catches the bug that made a live
// Ask read the gzip magic byte as JSON.
func TestSearchDecompressesGzip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("Accept-Encoding %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(`{"results":[{"title":"Map","url":"https://example.com/map","source":"example.com","thumbnail":{"src":"https://imgs.search.brave.com/a","width":500,"height":400},"properties":{"url":"https://example.com/map.jpg","width":1000,"height":800}}]}`))
		_ = gz.Close()
	}))
	defer srv.Close()

	c := New("k", srv.Client())
	c.BaseURL = srv.URL
	got, err := c.Search(context.Background(), "map")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Map" {
		t.Fatalf("got %+v", got)
	}
}
