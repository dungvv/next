package unfurl

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/internal/api/ssrf"
)

// maxResponseSize mirrors MAX_RESPONSE_SIZE for the generic proxy: 2 MB.
const maxResponseSize = 2 * 1024 * 1024

// Deps for the unfurl router.
type Deps struct {
	Fetcher Fetcher
	// Client is the SSRF-safe client used by the generic /proxy endpoint.
	Client *http.Client
}

// NewDeps wires the production fetcher and proxy client.
func NewDeps() Deps {
	f := NewHTTPFetcher()
	return Deps{Fetcher: f, Client: f.Client}
}

type bulkBody struct {
	URLList []string `json:"url_list"`
}

type bulkResponse struct {
	Responses []*Response `json:"responses"`
}

// Register installs the unfurl api_router content on r: GET /unfurl,
// POST /unfurl/bulk, GET /proxy. Mounted at root and under the /unfurl
// gateway prefix by the caller (mirroring mount_at_root_and_prefix).
// withProxy=false skips /proxy (used at root where the image proxy owns it).
func (d Deps) Register(r chi.Router, withProxy bool) {
	r.Post("/unfurl/bulk", d.bulkHandler)
	r.Get("/unfurl", d.getHandler)
	r.Get("/unfurl/", d.getHandler)
	if withProxy {
		r.Get("/proxy", d.proxyHandler)
		r.Get("/proxy/", d.proxyHandler)
	}
}

// getHandler ports unfurl_router's GET /unfurl: 200 JSON on success, 500 with
// a JSON `null` body on failure.
func (d Deps) getHandler(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	resp, err := Unfurl(r.Context(), d.Fetcher, rawURL)
	if err != nil {
		slog.Warn("unfurl: unfurl failed", "url", rawURL, "err", err)
		httpx.WriteJSON(w, http.StatusInternalServerError, nil)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// bulkHandler ports get_bulk_unfurl_handler: POST /unfurl/bulk.
func (d Deps) bulkHandler(w http.ResponseWriter, r *http.Request) {
	var body bulkBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bulkResponse{
		Responses: FetchLinks(r.Context(), d.Fetcher, body.URLList),
	})
}

// proxyHandler ports api/proxy.rs::proxy_request_handler: forward whitelisted
// request headers, manual redirect loop with per-hop SSRF checks, whitelist
// response headers, 2 MB cap.
func (d Deps) proxyHandler(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	u, ferr := ssrf.ValidateURL(rawURL)
	if ferr != nil {
		writeFetchErr(w, ferr)
		return
	}
	fwd := forwardedRequestHeaders(r.Header)

	resp, ferr := d.fetchUpstream(r.Context(), u, fwd)
	if ferr != nil {
		writeFetchErr(w, ferr)
		return
	}
	defer resp.Body.Close()

	if ferr := ssrf.CheckContentLength(resp, maxResponseSize, rawURL); ferr != nil {
		writeFetchErr(w, ferr)
		return
	}

	for k, vs := range resp.Header {
		if isAllowedResponseHeader(k) {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
	}
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)

	body := ssrf.MaxBytesBody(resp, maxResponseSize, rawURL)
	defer body.Close()
	if _, err := io.Copy(w, body); err != nil {
		slog.Warn("unfurl proxy: stream aborted", "url", rawURL, "err", err)
	}
}

func (d Deps) fetchUpstream(ctx context.Context, u *url.URL, headers http.Header) (*http.Response, *ssrf.FetchError) {
	redirects := ssrf.MaxRedirects
	for {
		if ferr := ssrf.AssertNotInternal(ctx, u); ferr != nil {
			return nil, ferr
		}
		resp, ferr := ssrf.SendRequest(ctx, d.Client, u, headers)
		if ferr != nil {
			return nil, ferr
		}
		if ssrf.IsRedirect(resp.StatusCode) {
			if redirects == 0 {
				resp.Body.Close()
				return nil, &ssrf.FetchError{Kind: ssrf.ErrUpstreamRedirect,
					Message: fmt.Sprintf("exceeded maximum of %d redirects", ssrf.MaxRedirects)}
			}
			next, ferr := ssrf.RedirectTarget(u, resp)
			resp.Body.Close()
			if ferr != nil {
				return nil, ferr
			}
			slog.Debug("unfurl proxy: following redirect", "from", u, "to", next)
			redirects--
			u = next
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			status := resp.StatusCode
			resp.Body.Close()
			return nil, &ssrf.FetchError{Kind: ssrf.ErrUpstreamStatus, StatusCode: status,
				Message: fmt.Sprintf("upstream returned status %d", status)}
		}
		return resp, nil
	}
}

func forwardedRequestHeaders(h http.Header) http.Header {
	out := http.Header{}
	for _, name := range []string{"Accept", "Accept-Language", "User-Agent"} {
		if v := h.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	return out
}

func isAllowedResponseHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Cache-Control", "Content-Encoding", "Content-Language",
		"Content-Type", "Etag", "Expires", "Last-Modified":
		return true
	}
	return false
}

func writeFetchErr(w http.ResponseWriter, e *ssrf.FetchError) {
	httpx.Error(w, e.HTTPStatus(), e.Error())
}
