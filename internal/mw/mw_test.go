package mw

import (
	"errors"
	"io"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
)

// fakeDoer returns a fresh response (status+body) on every call, so retries that
// consume the body still work.
type fakeDoer struct {
	status int
	body   string
	err    error
	calls  int
}

func (f *fakeDoer) Do(*fhttp.Request) (*fhttp.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &fhttp.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

// A word MW genuinely has no page for (e.g. "ostracization") returns a real
// HTTP 404. That must read as "no pronunciation" — entries nil, NO error — so
// the UI shows the LLM's AI-made fallback, not the red "unavailable" badge.
// A Cloudflare block / 5xx / network failure must still surface as an error.
func TestFetchEntriesStatusClassification(t *testing.T) {
	cases := []struct {
		name      string
		doer      *fakeDoer
		wantErr   bool
		wantCalls int // 404/200 are terminal (1 call); 403/503 retry to the cap (4)
	}{
		{"mw 404 is not-found, not an outage", &fakeDoer{status: 404}, false, 1},
		{"served page, no pron", &fakeDoer{status: 200, body: "<html>no entries here</html>"}, false, 1},
		{"cloudflare 403 block is an outage", &fakeDoer{status: 403}, true, 4},
		{"503 is an outage", &fakeDoer{status: 503}, true, 4},
		{"network failure is an outage", &fakeDoer{err: io.ErrUnexpectedEOF}, true, 4},
		{"unexpected 500 is an outage", &fakeDoer{status: 500}, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{http: tc.doer} // nil cache → no caching, every call hits the doer
			entries, err := c.FetchEntries("someword")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (entries=%v)", entries)
				}
				var ue *UnavailableError
				if !errors.As(err, &ue) {
					t.Fatalf("want *UnavailableError, got %T: %v", err, err)
				}
			} else {
				if err != nil {
					t.Fatalf("want nil error, got %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("want no entries, got %d", len(entries))
				}
			}
			if tc.doer.calls != tc.wantCalls {
				t.Fatalf("Do called %d times, want %d", tc.doer.calls, tc.wantCalls)
			}
		})
	}
}

// MW's play-pron anchor holds an inline speaker SVG with an accessibility
// <title>. tagRe strips tags but keeps their text, so without svgRe that label
// ("How to pronounce bureaucracy (audio)") would end up in the respelling. This
// fixture is the real anchor shape MW serves; the IPA must come out clean, with
// the audio label dropped along with the whole SVG subtree.
func TestParseProns_DropsSvgTitleLabel(t *testing.T) {
	// Faithful to the live entry structure: header (hword / parts-of-speech /
	// syllables / prons) is bounded by the definitions section (class="vg").
	const page = `<div id="dictionary-entry-1" class="entry-word-section-container">
    <div class="row entry-header simple-definitions-header"><div class="col-12">
      <div class="entry-header-content d-flex flex-wrap align-items-baseline flex-row mb-0">
        <h1 class="hword">bureaucracy</h1>
        <h2 class="parts-of-speech"><a class="important-blue-link" href="/dictionary/noun">noun</a></h2>
      </div>
      <div class="row entry-attr mb-3 mt-2"><div class="col word-syllables-prons-header-content flex-wrap">
        <span class="word-syllables-entry">bu·&#8203;reau·&#8203;cra·&#8203;cy</span>
        <span class="prons-entries-list-inline mb-1">
          <a class="play-pron-v2 text-decoration-none prons-entry-list-item d-inline-block badge mw-badge-gray-300" data-lang="en_us" data-file="bureau03" data-dir="b" href="/dictionary/bureaucracy?pronunciation" title="How&#x20;to&#x20;pronounce&#x20;bureaucracy&#x20;&#x28;audio&#x29;">byu̇-ˈrä-krə-sē&nbsp;<svg width="15" height="13" viewBox="0 0 15 13" class="svg replaced-svg" role="img"><title>How to pronounce bureaucracy (audio)</title>
<path fill-rule="evenodd" d="M13.513 6.34363Z" fill="#4A7D95"/>
</svg>
</a>
        </span>
      </div></div>
    </div></div>
    <div class="vg"><div class="sb">the complex rules and endless paperwork of a big government</div></div>
  </div>`

	entries := parseEntries(page, "bureaucracy")
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Pos != "noun" {
		t.Errorf("pos = %q, want %q", e.Pos, "noun")
	}
	if len(e.Prons) != 1 {
		t.Fatalf("want 1 pron, got %d: %+v", len(e.Prons), e.Prons)
	}
	p := e.Prons[0]
	if p.IPA != "byu̇-ˈrä-krə-sē" {
		t.Errorf("ipa = %q, want %q (SVG <title> label must be dropped)", p.IPA, "byu̇-ˈrä-krə-sē")
	}
	if strings.Contains(p.IPA, "How to pronounce") || strings.Contains(p.IPA, "audio") {
		t.Errorf("ipa still contains the leaked audio label: %q", p.IPA)
	}
	if p.MP3 != "https://media.merriam-webster.com/audio/prons/en/us/mp3/b/bureau03.mp3" {
		t.Errorf("mp3 = %q, want the bureau03 en/us clip", p.MP3)
	}
}

// mapCache is a trivial in-memory Cache (it ignores the TTL).
type mapCache map[string]string

func (m mapCache) Get(k string) (string, bool) { v, ok := m[k]; return v, ok }
func (m mapCache) Set(k, v string, _ int64)    { m[k] = v }

// The real "flyer" page: its whole entry is a spelling cross-reference to
// "flier" and carries no pronunciation of its own.
const flyerPage = `<div id="dictionary-entry-1" class="entry-word-section-container">
		<h1 class="hword">flyer</h1>
		<p class="cxl-ref">          <span class="cxl">less common spelling of</span>
		     <a href="flier" class="cxt"><span class="text-uppercase">flier</span></a>      </p>
		</div>`

// flierPage is the cross-reference target, with a pronunciation of its own.
const flierPage = `<div id="dictionary-entry-1" class="entry-word-section-container">
		<h1 class="hword">flier</h1>
		<h2 class="parts-of-speech"><a href="/dictionary/noun">noun</a></h2>
		<a class="play-pron-v2" data-lang="en_us" data-file="flier01" data-dir="f" href="#">ˈflī(-ə)r</a>
		<div class="vg">one that flies</div>
		</div>`

func TestCrossRefTarget(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{"less common spelling of", flyerPage, "flier"},
		{"variant spelling of", `<p class="cxl-ref"><span class="cxl">variant spelling of</span><a href="/dictionary/colour" class="cxt">colour</a></p>`, "colour"},
		{"variant of", `<p class="cxl-ref"><span class="cxl">chiefly British variant of</span><a href="theatre" class="cxt">theatre</a></p>`, "theatre"},
		{"href with sense anchor", `<p class="cxl-ref"><span class="cxl">less common spelling of</span><a href="flier#h1" class="cxt">flier</a></p>`, "flier"},
		// "see"-style jumps and synonym cross-refs are NOT the same word — ignore.
		{"non-spelling see ref", `<p class="cxl-ref"><span class="cxl">see</span><a href="run" class="cxt">run</a></p>`, ""},
		{"no cross-ref at all", `<h1 class="hword">suddenly</h1>`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := crossRefTarget(tc.html); got != tc.want {
				t.Fatalf("crossRefTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

// routeDoer answers each request from the fakeDoer registered for its URL path
// (e.g. "/dictionary/flier"), so a variant page and its cross-reference target
// can be served differently. An unregistered path is a 404.
type routeDoer map[string]*fakeDoer

func (r routeDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	if d, ok := r[req.URL.Path]; ok {
		return d.Do(req)
	}
	return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// "flyer" is a bare spelling cross-reference to "flier". When MW refuses the
// "flier" page, that is an outage, not a verdict that "flyer" has no
// pronunciation: the error must surface (so /api/pron reports mw_unavailable)
// and nothing may be cached, so the next lookup retries the follow and resolves
// once MW serves the target again.
func TestFetchEntriesCrossRefFollowFailureNotCached(t *testing.T) {
	flier := &fakeDoer{status: 403}
	cache := mapCache{}
	c := &Client{cache: cache, http: routeDoer{
		"/dictionary/flyer": {status: 200, body: flyerPage},
		"/dictionary/flier": flier,
	}}

	entries, err := c.FetchEntries("flyer")
	var ue *UnavailableError
	if !errors.As(err, &ue) || ue.Status != 403 {
		t.Fatalf("want *UnavailableError{403}, got entries=%v err=%v", entries, err)
	}
	if flier.calls != 4 {
		t.Fatalf("target fetched %d times, want 4 (all retries)", flier.calls)
	}
	for _, k := range []string{"mw:flyer", "mw:flier"} {
		if v, ok := cache[k]; ok {
			t.Fatalf("outage cached under %s: %s", k, v)
		}
	}

	flier.status, flier.body = 200, flierPage
	entries, err = c.FetchEntries("flyer")
	if err != nil {
		t.Fatalf("retry after outage: %v", err)
	}
	if len(entries) != 1 || len(entries[0].Prons) != 1 || entries[0].Prons[0].IPA != "ˈflī(-ə)r" {
		t.Fatalf("want flier's pron, got %+v", entries)
	}
	if _, ok := cache["mw:flyer"]; !ok {
		t.Fatalf("resolved variant not cached under mw:flyer")
	}
}

// A cross-reference target that MW genuinely has no page for (404) is a real
// "no pronunciation": no error, and the variant is cached as empty.
func TestFetchEntriesCrossRefTargetWithoutPronIsCached(t *testing.T) {
	cache := mapCache{}
	c := &Client{cache: cache, http: routeDoer{
		"/dictionary/flyer": {status: 200, body: flyerPage},
	}}
	entries, err := c.FetchEntries("flyer")
	if err != nil || len(entries) != 0 {
		t.Fatalf("want no entries and no error, got entries=%v err=%v", entries, err)
	}
	if got, ok := c.readCache("flyer"); !ok || len(got) != 0 {
		t.Fatalf("want an empty cached result for flyer, got %v (hit=%v)", got, ok)
	}
}
