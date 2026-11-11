// Package brave is the client for Brave's Image Search API. It is deliberately
// small: one request per search, a short timeout, and the filtering /api/ask
// needs before it shows candidates to a model. The API key stays in
// ai-service.env; nothing here is exposed to the extension.
package brave

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is Brave's API root. Tests replace it with an httptest
	// server.
	DefaultBaseURL = "https://api.search.brave.com"
	// SearchCount is the number of candidates one request asks for. The owner
	// fixed this at 10 on 2026-11-10.
	SearchCount = 10

	searchTimeout   = 5 * time.Second
	downloadTimeout = 5 * time.Second
	maxResponse     = 8 << 20
	maxImageBytes   = 4 << 20
)

// stockDomains are image sites whose results are almost always watermarked
// previews. They are not useful in the Ask card, and a model choosing from
// metadata alone cannot see the watermark. Match against the source domain and
// the page URL's hostname.
var stockDomains = []string{
	"shutterstock.com",
	"istockphoto.com",
	"gettyimages.com",
	"dreamstime.com",
	"alamy.com",
	"stock.adobe.com",
	"depositphotos.com",
	"123rf.com",
	"vecteezy.com",
	"canstockphoto.com",
	"bigstockphoto.com",
	"pond5.com",
}

// Candidate is one filtered Brave image result. ID is assigned by the Ask
// handler, not Brave; it is the handle the model passes to show_images.
type Candidate struct {
	ID          string
	Title       string
	Source      string
	PageURL     string
	ThumbURL    string
	OriginalURL string
	Width       int
	Height      int
	Confidence  string
}

// Client sends image searches to Brave. HTTP should have no overall timeout;
// Search and Download add their own short deadlines.
type Client struct {
	APIKey    string
	BaseURL   string
	ThumbHost string
	HTTP      *http.Client
}

// New builds a Brave client. An empty key is allowed here; Search reports it.
func New(apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{APIKey: apiKey, BaseURL: DefaultBaseURL, ThumbHost: "imgs.search.brave.com", HTTP: hc}
}

// Error is an HTTP failure from Brave. The status is kept so the caller can
// distinguish a bad key (401), a bad query (422), and a rate limit (429).
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Brave returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("Brave returned HTTP %d: %s", e.Status, e.Message)
}

type searchResponse struct {
	Query struct {
		Original          string `json:"original"`
		Altered           string `json:"altered"`
		ShowStrictWarning bool   `json:"show_strict_warning"`
	} `json:"query"`
	Results []struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Source      string `json:"source"`
		PageFetched string `json:"page_fetched"`
		Thumbnail   struct {
			Src    string `json:"src"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"thumbnail"`
		Properties struct {
			URL         string `json:"url"`
			Placeholder string `json:"placeholder"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
		} `json:"properties"`
		Confidence string `json:"confidence"`
	} `json:"results"`
	Extra struct {
		MightBeOffensive bool `json:"might_be_offensive"`
	} `json:"extra"`
}

// Search runs one image search and returns filtered candidates in Brave's
// order. It asks for 10, drops results a reader would not want to see, and
// caps the result at 10.
func (c *Client) Search(ctx context.Context, query string) ([]Candidate, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("image search needs a query")
	}
	if len(query) > 400 || len(strings.Fields(query)) > 50 {
		return nil, fmt.Errorf("image search query is too long (max 400 characters and 50 words)")
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("BRAVE_SEARCH_API_KEY is not set")
	}

	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	u, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/res/v1/images/search")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", fmt.Sprintf("%d", SearchCount))
	q.Set("country", "ALL")
	q.Set("search_lang", "en")
	q.Set("safesearch", "strict")
	q.Set("spellcheck", "false")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// Do not set Accept-Encoding by hand: net/http adds gzip and transparently
	// decompresses only when it owns the header. Setting it here made Search
	// read the compressed bytes as JSON ("invalid character '\x1f'").
	req.Header.Set("User-Agent", "comart/0.1")
	req.Header.Set("X-Subscription-Token", c.APIKey)

	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("image search timed out")
		}
		return nil, fmt.Errorf("image search failed: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if res.StatusCode != http.StatusOK {
		return nil, &Error{Status: res.StatusCode, Message: braveError(raw)}
	}

	var body searchResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("image search returned invalid JSON: %w", err)
	}
	if body.Query.Altered != "" {
		log.Printf("brave[images]: query %q altered to %q", query, body.Query.Altered)
	}
	if body.Query.ShowStrictWarning {
		log.Printf("brave[images]: strict SafeSearch blocked results for %q", query)
	}

	out := make([]Candidate, 0, SearchCount)
	seen := make(map[string]bool, SearchCount)
	for _, r := range body.Results {
		thumb := strings.TrimSpace(r.Thumbnail.Src)
		page := strings.TrimSpace(r.URL)
		original := strings.TrimSpace(r.Properties.URL)
		if !isHTTPS(thumb) || !isHTTPS(page) {
			continue
		}
		source := strings.TrimSpace(r.Source)
		if source == "" {
			source = hostname(page)
		}
		if isStock(source) || isStock(hostname(page)) {
			continue
		}
		width, height := r.Properties.Width, r.Properties.Height
		if width <= 0 || height <= 0 {
			width, height = r.Thumbnail.Width, r.Thumbnail.Height
		}
		if width > 0 && height > 0 {
			if width < 400 || height < 250 {
				continue
			}
			ratio := float64(width) / float64(height)
			if ratio < 0.35 || ratio > 2.8 {
				continue
			}
		}
		key := original
		if key == "" {
			key = thumb
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if !isHTTPS(original) {
			original = ""
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = "Image result for " + query
		}
		out = append(out, Candidate{
			Title:       title,
			Source:      source,
			PageURL:     page,
			ThumbURL:    thumb,
			OriginalURL: original,
			Width:       width,
			Height:      height,
			Confidence:  strings.TrimSpace(r.Confidence),
		})
		if len(out) >= SearchCount {
			break
		}
	}
	return out, nil
}

// Download fetches a Brave-proxied thumbnail and returns it as a base64 image
// for Claude's tool result. Only imgs.search.brave.com is accepted: the model
// never controls this URL, but keeping the host narrow prevents the tool from
// becoming a generic fetcher.
func (c *Client) Download(ctx context.Context, rawURL string) (mediaType, data string, err error) {
	u, err := url.Parse(rawURL)
	host := c.ThumbHost
	if host == "" {
		host = "imgs.search.brave.com"
	}
	if err != nil || u.Scheme != "https" || u.Hostname() != host {
		return "", "", fmt.Errorf("not a Brave image URL")
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "comart/0.1")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("image download HTTP %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxImageBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(raw) > maxImageBytes {
		return "", "", fmt.Errorf("image is too large")
	}
	mediaType = strings.ToLower(strings.TrimSpace(strings.Split(res.Header.Get("Content-Type"), ";")[0]))
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = http.DetectContentType(raw)
	}
	switch mediaType {
	case "image/jpeg", "image/jpg":
		mediaType = "image/jpeg"
	case "image/png", "image/gif", "image/webp":
	default:
		return "", "", fmt.Errorf("unsupported image type %q", mediaType)
	}
	return mediaType, base64.StdEncoding.EncodeToString(raw), nil
}

func isHTTPS(raw string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "https://")
}

func hostname(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func isStock(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimPrefix(domain, "www.")
	for _, s := range stockDomains {
		if domain == s || strings.HasSuffix(domain, "."+s) {
			return true
		}
	}
	return false
}

func braveError(raw []byte) string {
	var e struct {
		Error struct {
			Detail string `json:"detail"`
			Code   string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Error.Detail != "" {
			return e.Error.Detail
		}
		if e.Error.Code != "" {
			return e.Error.Code
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}
