// Package mw is the Merriam-Webster pronunciation scraper: it fetches a word's
// page, parses out the per-entry pronunciations with POS resolution, and caches
// each result for 30 days via the Cache interface.
package mw

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Cache is the store for lookup results (the SQLite cache in production). Each
// value is a JSON string.
type Cache interface {
	Get(key string) (string, bool)
	Set(key, value string, ttlSeconds int64)
}

const (
	// UA must match the impersonated TLS profile (Chrome 146) — Cloudflare
	// cross-checks the User-Agent against the TLS/HTTP2 fingerprint.
	UA          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	cacheTTLMs  = int64(30 * 24 * 3600 * 1000)
	cacheTTLSec = int64(30 * 24 * 3600)
)

// --- types --------------------------------------------------------------------
//
// The JSON field names are part of the API: LookupResult is the /api/pron
// reply, Bundle is sent in the explain stream's pron-data event, and Entry and
// Pron are stored in cached rows. Do not rename them.

type Pron struct {
	IPA  string `json:"ipa"`
	Lang string `json:"lang"`
	Dir  string `json:"dir"`
	File string `json:"file"`
	MP3  string `json:"mp3"`
	WAV  string `json:"wav"`
}

type Entry struct {
	Hword     string `json:"hword"`
	Num       string `json:"num"`
	Pos       string `json:"pos"`
	Syllables string `json:"syllables"`
	Prons     []Pron `json:"prons"`
}

type Variant struct {
	Pos     string   `json:"pos"`
	PosList []string `json:"posList"`
	Num     string   `json:"num"`
	IPA     string   `json:"ipa"`
	MP3     string   `json:"mp3"`
}

type Bundle struct {
	Hword    string    `json:"hword"`
	Variants []Variant `json:"variants"`
}

type LookupResult struct {
	Hword     string    `json:"hword"`
	Pos       string    `json:"pos"`
	Num       string    `json:"num"`
	Syllables string    `json:"syllables"`
	IPA       string    `json:"ipa"`
	MP3       string    `json:"mp3"`
	Variants  []Variant `json:"variants"`
}

// UnavailableError means MW refused to serve the page (a non-OK status —
// usually Cloudflare's bot challenge — or a network failure). It is distinct
// from an empty result, which means MW has no pronunciation for the word
// (fetchEntries also turns a 404 into an empty result).
type UnavailableError struct{ Status int } // HTTP status, or 0 for a network failure

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("Merriam-Webster unreachable (status %d)", e.Status)
}

// --- regexes over MW's page markup --------------------------------------------

var (
	entryStartRe   = regexp.MustCompile(`<div id="dictionary-entry-(\d+)" class="entry-word-section-container">`)
	entryHeaderEnd = regexp.MustCompile(`class="row headword-row|<div class="vg|<section\b`)
	anchorRe       = regexp.MustCompile(`<a\b[^>]*class="[^"]*play-pron-v2[^"]*"[^>]*>([\s\S]*?)</a>`)
	attrRe         = regexp.MustCompile(`data-(lang|file|dir)="([^"]*)"`)
	tagRe          = regexp.MustCompile(`<[^>]+>`)
	// MW's inline speaker icon carries an accessibility label as an SVG
	// <title> — e.g. the play-pron anchor's inner HTML is
	// `byu̇-ˈrä-krə-sē&nbsp;<svg …><title>How to pronounce bureaucracy (audio)</title>…</svg>`.
	// tagRe strips the <svg>/<title>/<path> tags but NOT the <title>'s text, so
	// that label would otherwise leak into the respelling. Drop the whole SVG
	// subtree (tags and inner text) up front in clean(); no scraped field ever
	// wants an icon's a11y text.
	svgRe      = regexp.MustCompile(`(?s)<svg\b[^>]*>.*?</svg>`)
	hwordRe    = regexp.MustCompile(`<h1 class="hword">([^<]+)</h1>`)
	numBadgeRe = regexp.MustCompile(`<span class="badge entry-numbers-badge[^"]*">([^<]+)</span>`)
	posRe      = regexp.MustCompile(`<h2 class="parts-of-speech[^"]*"[^>]*>([\s\S]*?)</h2>`)
	syllRe     = regexp.MustCompile(`<span\b[^>]*class="[^"]*word-syllables-entry[^"]*"[^>]*>([\s\S]*?)</span>`)
	ureRe      = regexp.MustCompile(`<span\b[^>]*class="[^"]*\bure\b[^"]*"[^>]*>([\s\S]*?)</span>`)
	uroFlRe    = regexp.MustCompile(`<span\b[^>]*class="[^"]*\bfl\b[^"]*"[^>]*>([\s\S]*?)</span>`)
	// A "this spelling points at another headword" cross-reference, e.g. the
	// "flyer" page whose whole entry is `<p class="cxl-ref"><span class="cxl">less
	// common spelling of</span><a href="flier" class="cxt">flier</a></p>` and
	// which carries no pronunciation of its own. Used to follow such a page to the
	// canonical spelling (see crossRefTarget / fetchEntries).
	cxlRefRe   = regexp.MustCompile(`(?s)<p class="cxl-ref">(.*?)</p>`)
	cxlTextRe  = regexp.MustCompile(`(?s)<span class="cxl">(.*?)</span>`)
	cxtTagRe   = regexp.MustCompile(`<a\b[^>]*class="[^"]*\bcxt\b[^"]*"[^>]*>`)
	hrefAttrRe = regexp.MustCompile(`href="([^"]*)"`)
	numDecRe   = regexp.MustCompile(`&#(\d+);`)
	numHexRe   = regexp.MustCompile(`&#x([0-9a-fA-F]+);`)
)

// --- Client -------------------------------------------------------------------

// httpDoer is the one method fetchPage needs from the TLS client. Narrowing the
// field to this interface (the real tls_client.HttpClient satisfies it) lets a
// test inject a fake transport without reaching the network.
type httpDoer interface {
	Do(req *fhttp.Request) (*fhttp.Response, error)
}

type Client struct {
	cache Cache
	http  httpDoer
}

func New(cache Cache) *Client {
	// www.merriam-webster.com sits behind Cloudflare's bot challenge, which 403s
	// a plain Go net/http client (and even headless browsers). A TLS/HTTP2
	// fingerprint impersonating current Chrome is served the real page. The
	// profile must be recent — older Chrome fingerprints get flagged. If lookups
	// start returning mw_unavailable, bump the profile together with UA and
	// browserHeaders. A cookie jar lets Cloudflare's clearance cookie
	// (cf_clearance) persist across lookups, so once the client is "warmed" the
	// challenge rate drops.
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(20),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	)
	if err != nil {
		// Should not happen with valid options; fall back to a bare client so
		// the process still starts (MW lookups will just report unavailable).
		client, _ = tls_client.NewHttpClient(tls_client.NewNoopLogger())
	}
	return &Client{cache: cache, http: client}
}

// browserHeaders is the Chrome request header set (with explicit ordering) that
// accompanies the impersonated fingerprint.
func browserHeaders() fhttp.Header {
	return fhttp.Header{
		"sec-ch-ua":                 {`"Chromium";v="146", "Google Chrome";v="146", "Not?A_Brand";v="99"`},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Windows"`},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {UA},
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"},
		"sec-fetch-site":            {"none"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-user":            {"?1"},
		"sec-fetch-dest":            {"document"},
		"accept-language":           {"en-US,en;q=0.9"},
		fhttp.HeaderOrderKey: {
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests",
			"user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user",
			"sec-fetch-dest", "accept-language",
		},
	}
}

// --- HTML helpers -------------------------------------------------------------

func decodeEntities(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = numDecRe.ReplaceAllStringFunc(s, func(m string) string {
		n := numDecRe.FindStringSubmatch(m)[1]
		code, err := strconv.Atoi(n)
		if err != nil {
			return m
		}
		return string(rune(code))
	})
	s = numHexRe.ReplaceAllStringFunc(s, func(m string) string {
		h := numHexRe.FindStringSubmatch(m)[1]
		code, err := strconv.ParseInt(h, 16, 32)
		if err != nil {
			return m
		}
		return string(rune(code))
	})
	return s
}

func clean(node string) string {
	node = svgRe.ReplaceAllString(node, "")
	stripped := tagRe.ReplaceAllString(node, "")
	decoded := decodeEntities(stripped)
	decoded = strings.ReplaceAll(decoded, " ", " ")
	return strings.TrimSpace(decoded)
}

func audioURL(lang, dir, file, format string) string {
	l := strings.ReplaceAll(lang, "_", "/")
	return "https://media.merriam-webster.com/audio/prons/" + l + "/" + format + "/" + dir + "/" + file + "." + format
}

// encodeURIComponent escapes a word for an MW page URL the same way as
// JavaScript's encodeURIComponent.
func encodeURIComponent(s string) string {
	const safe = "-_.!~*'()"
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune(safe, r) {
			b.WriteRune(r)
		} else {
			for _, by := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", by)
			}
		}
	}
	return b.String()
}

func pageURL(word string) string {
	return "https://www.merriam-webster.com/dictionary/" + encodeURIComponent(word)
}

// --- parsing ------------------------------------------------------------------

func parseProns(block string) []Pron {
	seen := map[string]bool{}
	var out []Pron
	for _, m := range anchorRe.FindAllStringSubmatch(block, -1) {
		chunk := m[0]
		attrs := map[string]string{}
		for _, a := range attrRe.FindAllStringSubmatch(chunk, -1) {
			attrs[a[1]] = a[2]
		}
		if attrs["lang"] == "" || attrs["file"] == "" || attrs["dir"] == "" {
			continue
		}
		ipa := clean(m[1])
		key := attrs["lang"] + "\x00" + attrs["dir"] + "\x00" + attrs["file"] + "\x00" + ipa
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Pron{
			IPA:  ipa,
			Lang: attrs["lang"],
			Dir:  attrs["dir"],
			File: attrs["file"],
			MP3:  audioURL(attrs["lang"], attrs["dir"], attrs["file"], "mp3"),
			WAV:  audioURL(attrs["lang"], attrs["dir"], attrs["file"], "wav"),
		})
	}
	return out
}

func parseUros(htmlText, fallbackWord string) []Entry {
	wanted := strings.TrimSpace(strings.ToLower(fallbackWord))
	if wanted == "" {
		return nil
	}
	locs := ureRe.FindAllStringSubmatchIndex(htmlText, -1)
	if len(locs) == 0 {
		return nil
	}
	var out []Entry
	for i, loc := range locs {
		word := clean(htmlText[loc[2]:loc[3]])
		if strings.ToLower(word) != wanted {
			continue
		}
		start := loc[0]
		end := min(start+5000, len(htmlText))
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := htmlText[start:end]
		prons := parseProns(block)
		if len(prons) == 0 {
			continue
		}
		pos := ""
		if fl := uroFlRe.FindStringSubmatch(block); fl != nil {
			pos = clean(fl[1])
		}
		out = append(out, Entry{Hword: word, Pos: pos, Prons: prons})
	}
	return out
}

func parseEntries(htmlText, fallbackWord string) []Entry {
	starts := entryStartRe.FindAllStringIndex(htmlText, -1)
	if len(starts) == 0 {
		if uros := parseUros(htmlText, fallbackWord); len(uros) > 0 {
			return uros
		}
		if prons := parseProns(htmlText); len(prons) > 0 {
			return []Entry{{Hword: fallbackWord, Prons: prons}}
		}
		return nil
	}

	var entries []Entry
	lastHword := fallbackWord
	for i := range starts {
		start := starts[i][0]
		end := len(htmlText)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		block := htmlText[start:end]

		var header string
		if cap := entryHeaderEnd.FindStringIndex(block); cap != nil {
			header = block[:cap[0]]
		} else {
			header = block[:min(6000, len(block))]
		}

		if h := hwordRe.FindStringSubmatch(header); h != nil {
			lastHword = clean(h[1])
		}
		var num, pos, syll string
		if n := numBadgeRe.FindStringSubmatch(header); n != nil {
			num = clean(n[1])
		}
		if p := posRe.FindStringSubmatch(header); p != nil {
			pos = clean(p[1])
		}
		if s := syllRe.FindStringSubmatch(header); s != nil {
			syll = clean(s[1])
		}

		prons := parseProns(header)
		// MW omits a sibling entry's pron when it shares an IPA with a prior
		// entry — inherit the previous entry's prons rather than grabbing a
		// run-on derivative's anchor from this entry's body.
		if len(prons) == 0 && len(entries) > 0 && len(entries[len(entries)-1].Prons) > 0 {
			prons = entries[len(entries)-1].Prons
		}
		entries = append(entries, Entry{Hword: lastHword, Num: num, Pos: pos, Syllables: syll, Prons: prons})
	}

	// If the looked-up word is a run-on derivative (its own hword never appears
	// among the parsed entries), substitute the uro entries instead.
	wanted := strings.TrimSpace(strings.ToLower(fallbackWord))
	if wanted != "" {
		found := false
		for _, e := range entries {
			if strings.ToLower(e.Hword) == wanted {
				found = true
				break
			}
		}
		if !found {
			if uros := parseUros(htmlText, fallbackWord); len(uros) > 0 {
				return uros
			}
		}
	}
	return entries
}

// --- cache --------------------------------------------------------------------

type envelope struct {
	T       int64   `json:"t"`
	Entries []Entry `json:"entries"`
}

// cacheKey is the lowercased word with each rune outside [a-z0-9-] written as
// "_<hex>". Keep the format stable: a new one turns every cached row into a
// miss.
func cacheKey(word string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(word) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteString("_" + strconv.FormatInt(int64(r), 16))
		}
	}
	return b.String()
}

func (c *Client) readCache(word string) ([]Entry, bool) {
	if c.cache == nil {
		return nil, false
	}
	raw, ok := c.cache.Get("mw:" + cacheKey(word))
	if !ok {
		return nil, false
	}
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, false
	}
	if time.Now().UnixMilli()-env.T > cacheTTLMs {
		return nil, false
	}
	return env.Entries, true
}

func (c *Client) writeCache(word string, entries []Entry) {
	if c.cache == nil {
		return
	}
	if entries == nil {
		entries = []Entry{}
	}
	b, err := json.Marshal(envelope{T: time.Now().UnixMilli(), Entries: entries})
	if err != nil {
		return
	}
	c.cache.Set("mw:"+cacheKey(word), string(b), cacheTTLSec)
}

// --- public API ---------------------------------------------------------------

// crossRefTarget detects a "spelling of" / "variant of" cross-reference page —
// e.g. "flyer", whose only content is "less common spelling of FLIER" and which
// carries no pronunciation of its own — and returns the target headword
// ("flier") so the caller can follow it for the real pronunciation. It returns
// "" when the page is not such a redirect. ONLY spelling/variant relations are
// followed: those are the SAME word under another spelling and so share MW's
// pronunciation. Other cross-references ("see", a synonym jump) are not the same
// word and are ignored.
func crossRefTarget(htmlText string) string {
	for _, block := range cxlRefRe.FindAllStringSubmatch(htmlText, -1) {
		body := block[1]
		label := ""
		if m := cxlTextRe.FindStringSubmatch(body); m != nil {
			label = strings.ToLower(clean(m[1]))
		}
		if !strings.Contains(label, "spelling of") && !strings.Contains(label, "variant of") {
			continue
		}
		tag := cxtTagRe.FindString(body)
		if tag == "" {
			continue
		}
		h := hrefAttrRe.FindStringSubmatch(tag)
		if h == nil {
			continue
		}
		target := h[1]
		// href is a bare headword ("flier") or a path ("/dictionary/flier"); take
		// the last path segment and drop any "#sense" / "?query" suffix.
		if i := strings.LastIndexByte(target, '/'); i >= 0 {
			target = target[i+1:]
		}
		if i := strings.IndexAny(target, "#?"); i >= 0 {
			target = target[:i]
		}
		if target = strings.TrimSpace(target); target != "" {
			return target
		}
	}
	return ""
}

func (c *Client) FetchEntries(word string) ([]Entry, error) {
	return c.fetchEntries(word, 0)
}

// fetchEntries fetches and parses a word's pron-bearing entries. When a page is
// a bare spelling/variant cross-reference with no pronunciation of its own (e.g.
// "flyer" → "less common spelling of flier"), it follows that reference once to
// the canonical headword so the variant spelling still resolves to a real
// pronunciation. depth caps the follow chain so a cross-reference cycle can't
// loop. The followed target's entries are cached under the original word too, so
// the redirect is paid at most once per word.
func (c *Client) fetchEntries(word string, depth int) ([]Entry, error) {
	w := strings.ToLower(strings.TrimSpace(word))
	if w == "" {
		return nil, nil
	}
	if cached, ok := c.readCache(w); ok {
		return cached, nil
	}
	html, err := c.fetchPage(w)
	if err != nil {
		// MW returns a genuine HTTP 404 for a word it simply has no page for
		// (e.g. "ostracization", which is not in the dictionary at all). That is
		// a definitive "no such word", NOT an outage: record it as an empty
		// result (cached so we don't re-hit MW for 30 days) and report it the
		// same as a served-but-pron-less page. The extension then shows the
		// model's own respelling, marked as AI-made, instead of the red
		// "pron unavailable" badge.
		// Every other failure (Cloudflare 403 block, 5xx, network) stays an
		// UnavailableError, so a transient outage is never cached as "no pron".
		var ue *UnavailableError
		if errors.As(err, &ue) && ue.Status == 404 {
			c.writeCache(w, nil)
			return nil, nil
		}
		return nil, err
	}
	var filtered []Entry
	for _, e := range parseEntries(html, w) {
		if len(e.Prons) > 0 {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 && depth < 2 {
		if target := crossRefTarget(html); target != "" && !strings.EqualFold(strings.TrimSpace(target), w) {
			ref, rerr := c.fetchEntries(target, depth+1)
			if rerr != nil {
				// The target's page could not be fetched (Cloudflare block, 5xx,
				// network) — an outage, not a verdict on this word. Surface it and
				// cache nothing, so the next lookup retries the follow instead of
				// serving 30 days of "no pronunciation".
				return nil, rerr
			}
			if len(ref) > 0 {
				c.writeCache(w, ref)
				return ref, nil
			}
		}
	}
	c.writeCache(w, filtered)
	return filtered, nil
}

// fetchPage GETs the MW page, retrying through Cloudflare's intermittent managed
// challenge. The challenge hits a fraction of requests (403); a retry on the
// same cookie-jar'd client almost always clears it, and once cleared the
// clearance cookie carries subsequent lookups. Only cold (uncached) words ever
// reach here, so a few retries per new word is cheap. Every non-OK status comes
// back as an UnavailableError; only fetchEntries decides that a 404 means "no
// such word".
func (c *Client) fetchPage(w string) (string, error) {
	const attempts = 4
	lastStatus := 0
	for i := 0; i < attempts; i++ {
		req, err := fhttp.NewRequest(fhttp.MethodGet, pageURL(w), nil)
		if err != nil {
			return "", &UnavailableError{Status: 0}
		}
		req.Header = browserHeaders()
		res, err := c.http.Do(req)
		if err != nil {
			lastStatus = 0
		} else if res.StatusCode >= 200 && res.StatusCode < 300 {
			body, rerr := io.ReadAll(res.Body)
			res.Body.Close()
			if rerr != nil {
				lastStatus = 0
			} else {
				return string(body), nil
			}
		} else {
			lastStatus = res.StatusCode
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			// 403 (challenge) / 429 / 503 are transient — retry. Any other
			// non-OK is terminal "unavailable".
			if lastStatus != 403 && lastStatus != 429 && lastStatus != 503 {
				return "", &UnavailableError{Status: lastStatus}
			}
		}
		if i < attempts-1 {
			time.Sleep(time.Duration(300*(i+1)) * time.Millisecond)
		}
	}
	return "", &UnavailableError{Status: lastStatus}
}

var posSyn = map[string]string{
	"n": "noun", "noun": "noun",
	"v": "verb", "verb": "verb",
	"adj": "adjective", "adjective": "adjective",
	"adv": "adverb", "adverb": "adverb",
	"prep": "preposition", "preposition": "preposition",
	"interj": "interjection", "interjection": "interjection",
	"conj": "conjunction", "conjunction": "conjunction",
	"pron": "pronoun", "pronoun": "pronoun",
}

func splitHead(s string) string {
	idx := strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '(' })
	if idx < 0 {
		return s
	}
	return s[:idx]
}

// pickEntry matches a POS hint from the LLM against the parsed entries.
func pickEntry(entries []Entry, posHint string) *Entry {
	if len(entries) == 0 {
		return nil
	}
	if posHint == "" {
		return &entries[0]
	}
	hint := strings.ToLower(strings.TrimSpace(posHint))
	head := splitHead(hint)
	for i := range entries {
		epos := strings.ToLower(entries[i].Pos)
		if epos == "" {
			continue
		}
		if epos == hint {
			return &entries[i]
		}
		if splitHead(epos) == head {
			return &entries[i]
		}
	}
	if canon, ok := posSyn[head]; ok {
		for i := range entries {
			if splitHead(strings.ToLower(entries[i].Pos)) == canon {
				return &entries[i]
			}
		}
	}
	return &entries[0]
}

// distinctVariants dedups prons by IPA across entries, preserving insertion
// order (Go maps don't, so we track order explicitly). It makes two passes —
// each entry's first pron, then the others — so every primary pronunciation
// comes before any alternate one.
func distinctVariants(entries []Entry) []Variant {
	order := []string{}
	byIPA := map[string]*Variant{}
	add := func(e Entry, p Pron) {
		v := &Variant{Pos: e.Pos, Num: e.Num, IPA: p.IPA, MP3: p.MP3, PosList: []string{}}
		if e.Pos != "" {
			v.PosList = []string{e.Pos}
		}
		byIPA[p.IPA] = v
		order = append(order, p.IPA)
	}
	fold := func(e Entry, p Pron) {
		ex := byIPA[p.IPA]
		if e.Pos != "" && !contains(ex.PosList, e.Pos) {
			ex.PosList = append(ex.PosList, e.Pos)
		}
	}
	for _, e := range entries {
		if len(e.Prons) == 0 {
			continue
		}
		p := e.Prons[0]
		if _, ok := byIPA[p.IPA]; ok {
			fold(e, p)
		} else {
			add(e, p)
		}
	}
	for _, e := range entries {
		for i := 1; i < len(e.Prons); i++ {
			p := e.Prons[i]
			if _, ok := byIPA[p.IPA]; ok {
				fold(e, p)
			} else {
				add(e, p)
			}
		}
	}
	out := make([]Variant, 0, len(order))
	for _, ipa := range order {
		out = append(out, *byIPA[ipa])
	}
	return out
}

func (c *Client) BundleVariants(word string) (*Bundle, error) {
	entries, err := c.FetchEntries(word)
	if err != nil {
		return nil, err
	}
	return bundleOf(entries), nil
}

// CachedBundle is BundleVariants answered from the cache alone, never the
// network. hit is false on a cache miss; a hit can still carry a nil bundle
// (MW has no pronunciation for the word).
func (c *Client) CachedBundle(word string) (b *Bundle, hit bool) {
	entries, hit := c.readCache(strings.ToLower(strings.TrimSpace(word)))
	if !hit {
		return nil, false
	}
	return bundleOf(entries), true
}

func bundleOf(entries []Entry) *Bundle {
	if len(entries) == 0 {
		return nil
	}
	variants := distinctVariants(entries)
	if len(variants) == 0 {
		return nil
	}
	return &Bundle{Hword: entries[0].Hword, Variants: variants}
}

func (c *Client) Lookup(word, posHint string) (*LookupResult, error) {
	entries, err := c.FetchEntries(word)
	if err != nil {
		return nil, err
	}
	e := pickEntry(entries, posHint)
	if e == nil || len(e.Prons) == 0 {
		return nil, nil
	}
	p := e.Prons[0]
	return &LookupResult{
		Hword:     e.Hword,
		Pos:       e.Pos,
		Num:       e.Num,
		Syllables: e.Syllables,
		IPA:       p.IPA,
		MP3:       p.MP3,
		Variants:  distinctVariants(entries),
	}, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
