// Package unfurl ports services/unfurl_service: URL unfurling via meta-tag
// scraping plus a generic safe proxy endpoint. The SSRF guard lives in
// internal/api/ssrf.
package unfurl

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/macro-inc/macro/internal/api/ssrf"
)

// maxHTMLSize mirrors MAX_HTML_SIZE: 1 MB cap for meta-tag extraction.
const maxHTMLSize = 1024 * 1024

// bulkConcurrency mirrors BULK_CONCURRENCY.
const bulkConcurrency = 16

const (
	requestTimeout = 8 * time.Second
	connectTimeout = 3 * time.Second
)

// Response mirrors unfurl::domain::models::GetUnfurlResponse.
type Response struct {
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Description *string `json:"description,omitempty"`
	ImageURL    *string `json:"image_url,omitempty"`
	FaviconURL  *string `json:"favicon_url,omitempty"`
}

// Fetcher is the UnfurlFetcher port: extract meta tags for a URL.
type Fetcher interface {
	FetchMetaTags(ctx context.Context, rawURL string) (map[string]string, error)
}

// HTTPFetcher is the production Fetcher (ReqwestUnfurlFetcher equivalent).
type HTTPFetcher struct {
	Client *http.Client
}

// NewHTTPFetcher builds a fetcher with an SSRF-safe client.
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{Client: ssrf.NewClient(requestTimeout, connectTimeout)}
}

// Unfurl fetches the URL and builds a Response (UnfurlServiceImpl::unfurl).
func Unfurl(ctx context.Context, f Fetcher, rawURL string) (*Response, error) {
	tags, err := f.FetchMetaTags(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	tags = appendOptimisticFavicon(tags, rawURL)
	return newResponse(rawURL, tags), nil
}

// FetchMetaTags ports extract_meta_tags_prod: validate → per-hop SSRF check →
// manual redirect loop → HTML-only → 1MB cap → parse.
func (f *HTTPFetcher) FetchMetaTags(ctx context.Context, rawURL string) (map[string]string, error) {
	u, ferr := ssrf.ValidateURL(rawURL)
	if ferr != nil {
		return nil, ferr
	}
	redirects := ssrf.MaxRedirects
	var resp *http.Response
	for {
		if ferr := ssrf.AssertNotInternal(ctx, u); ferr != nil {
			return nil, ferr
		}
		r, ferr := ssrf.SendRequest(ctx, f.Client, u, nil)
		if ferr != nil {
			return nil, ferr
		}
		if ssrf.IsRedirect(r.StatusCode) {
			if redirects == 0 {
				r.Body.Close()
				return nil, &ssrf.FetchError{Kind: ssrf.ErrUpstreamRedirect,
					Message: fmt.Sprintf("exceeded maximum of %d redirects", ssrf.MaxRedirects)}
			}
			next, ferr := ssrf.RedirectTarget(u, r)
			r.Body.Close()
			if ferr != nil {
				return nil, ferr
			}
			slog.Debug("unfurl: following redirect", "from", u, "to", next)
			redirects--
			u = next
			continue
		}
		if r.StatusCode < 200 || r.StatusCode > 299 {
			status := r.StatusCode
			r.Body.Close()
			return nil, &ssrf.FetchError{Kind: ssrf.ErrUpstreamStatus, StatusCode: status,
				Message: fmt.Sprintf("upstream returned status %d", status)}
		}
		resp = r
		break
	}
	defer resp.Body.Close()

	contentType := ssrf.ContentTypeOf(resp)
	if !isHTMLContentType(contentType) {
		return nil, &ssrf.FetchError{Kind: ssrf.ErrUnexpectedContentType,
			Message: "unexpected content-type: " + contentType}
	}
	if ferr := ssrf.CheckContentLength(resp, maxHTMLSize, rawURL); ferr != nil {
		return nil, ferr
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLSize+1))
	if err != nil {
		return nil, &ssrf.FetchError{Kind: ssrf.ErrResponseRead,
			Message: "error reading upstream response: " + err.Error()}
	}
	if len(body) > maxHTMLSize {
		return nil, &ssrf.FetchError{Kind: ssrf.ErrResponseTooLarge,
			Message: fmt.Sprintf("response exceeds max size of %d bytes", maxHTMLSize)}
	}

	return parseDocument(string(body), u)
}

func isHTMLContentType(ct string) bool {
	primary := ct
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		primary = ct[:i]
	}
	primary = strings.ToLower(strings.TrimSpace(primary))
	return primary == "text/html" || primary == "application/xhtml+xml"
}

// parseDocument ports the scraper-based parse_document: collect
// name/property meta tags, <title>, and a favicon from <link rel=*icon*>.
func parseDocument(htmlContent string, baseURL *url.URL) (map[string]string, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse document: %w", err)
	}
	tags := map[string]string{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "meta":
				name := attr(n, "name")
				prop := attr(n, "property")
				content := attr(n, "content")
				if name != "" && content != "" {
					tags["name:"+name] = content
				}
				if prop != "" && content != "" {
					tags["property:"+prop] = content
				}
			case "title":
				tags["title"] = innerText(n)
			case "link":
				rel := attr(n, "rel")
				href := attr(n, "href")
				if href == "" {
					break
				}
				for _, r := range strings.Fields(rel) {
					if strings.Contains(strings.ToLower(r), "icon") {
						if abs, err := baseURL.Parse(href); err == nil {
							tags["favicon"] = abs.String()
						} else {
							tags["favicon"] = href
						}
						return
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return tags, nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// innerText approximates scraper's inner_html for <title>: the Rust impl
// stores the element's inner HTML; for title elements that is the text.
func innerText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

// favicoURL returns <scheme>://<host>[:<port>]/favicon.ico.
func favicoURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("no host")
	}
	port := ""
	if u.Port() != "" {
		port = ":" + u.Port()
	}
	return u.Scheme + "://" + host + port + "/favicon.ico", nil
}

// appendOptimisticFavicon fills in the conventional /favicon.ico when the
// page didn't provide one.
func appendOptimisticFavicon(tags map[string]string, rawURL string) map[string]string {
	if _, ok := tags["favicon"]; !ok {
		if fav, err := favicoURL(rawURL); err == nil {
			tags["favicon"] = fav
		} else {
			slog.Debug("unfurl: could not form favicon url", "err", err)
		}
	}
	return tags
}

// newResponse ports GetUnfurlResponse::new.
func newResponse(rawURL string, tags map[string]string) *Response {
	title := ""
	if t, ok := ParseCustomTitle(rawURL); ok {
		title = t
	} else {
		for _, k := range []string{"property:og:title", "property:og:site_name", "title"} {
			if v, ok := tags[k]; ok {
				title = v
				break
			}
		}
		if title == "" {
			title = rawURL
		}
	}

	resp := &Response{URL: rawURL, Title: title}
	if v, ok := tags["property:og:description"]; ok {
		resp.Description = &v
	}
	if v, ok := tags["property:og:image"]; ok {
		resp.ImageURL = &v
	}
	if v, ok := tags["favicon"]; ok {
		fav := v
		if base, err := url.Parse(rawURL); err == nil {
			if joined, err := base.Parse(fav); err == nil {
				fav = joined.String()
			}
		}
		resp.FaviconURL = &fav
	}
	return resp
}

// FetchLinks ports fetch_links_async: buffered(16) fan-out preserving input
// order, nil for failures.
func FetchLinks(ctx context.Context, f Fetcher, links []string) []*Response {
	out := make([]*Response, len(links))
	sem := make(chan struct{}, bulkConcurrency)
	var wg sync.WaitGroup
	for i, link := range links {
		wg.Add(1)
		go func(i int, link string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp, err := Unfurl(ctx, f, link)
			if err != nil {
				slog.Warn("unfurl: bulk unfurl failed for url", "url", link, "err", err)
				return
			}
			out[i] = resp
		}(i, link)
	}
	wg.Wait()
	return out
}
