// Package imageproxy ports services/image_proxy_service: fetch a remote image
// through an SSRF-guarded client and stream it back with long-lived caching
// headers.
//
// Extension over the Rust service: optional `w`, `h` (max width/height, fit
// preserving aspect) and `format` (jpeg|png|gif|bmp|tiff|webp-decode only)
// query params resize/re-encode via disintegration/imaging. Without them the
// response is a byte-for-byte stream passthrough, as in Rust.
// Format limits: decode jpeg/png/gif/bmp/tiff (+webp via x/image). HEIC and
// AVIF are not supported by pure-Go imaging.
package imageproxy

import (
	"bytes"
	"context"
	"fmt"
	_ "image/gif" // register gif decode (imaging does not init it)
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	"github.com/go-chi/chi/v5"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp" // decode only; encode unsupported

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/internal/api/ssrf"
)

// 10 MB max image size.
const maxImageSize = 10 * 1024 * 1024

const (
	requestTimeout = 15 * time.Second
	connectTimeout = 5 * time.Second
)

// upstreamUserAgent mirrors UPSTREAM_USER_AGENT — a browser-like string that
// clears strict WAFs that 403 requests with no User-Agent.
const upstreamUserAgent = "Mozilla/5.0 (compatible; MacroImageProxy/1.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// Deps for the image proxy router.
type Deps struct {
	Client *http.Client // built with ssrf.NewClient
}

// NewDeps builds the SSRF-guarded upstream client.
func NewDeps() Deps {
	return Deps{Client: ssrf.NewClient(requestTimeout, connectTimeout)}
}

// Register installs GET /proxy (and /proxy/) on r. The caller mounts these
// at root and under the /image-proxy gateway prefix.
func (d Deps) Register(r chi.Router) {
	r.Get("/proxy", d.proxyHandler)
	r.Get("/proxy/", d.proxyHandler)
}

func (d Deps) proxyHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rawURL := r.URL.Query().Get("url")
	u, ferr := ssrf.ValidateURL(rawURL)
	if ferr != nil {
		writeErr(w, ferr)
		return
	}

	// Optional transform params (Go port extension).
	var tf transformParams
	tf.parse(r.URL.Query())

	// Primary attempt with the browser-like UA; a minority of hosts reject it,
	// so retry once with no UA on retryable failures.
	resp, contentType, ferr := d.fetchUpstream(r.Context(), u, true)
	if ferr != nil && isRetryableWithoutUA(ferr) {
		slog.Info("imageproxy: retrying upstream fetch without User-Agent",
			"url", rawURL, "err", ferr)
		resp, contentType, ferr = d.fetchUpstream(r.Context(), u, false)
	}
	if ferr != nil {
		writeErr(w, ferr)
		return
	}
	defer resp.Body.Close()

	if ferr := ssrf.CheckContentLength(resp, maxImageSize, rawURL); ferr != nil {
		writeErr(w, ferr)
		return
	}

	body := ssrf.MaxBytesBody(resp, maxImageSize, rawURL)
	defer body.Close()

	if tf.enabled() {
		data, err := io.ReadAll(body)
		if err != nil {
			writeErr(w, errf(ssrf.ErrResponseRead, "error reading upstream response: %s", err))
			return
		}
		out, outCT, err := tf.apply(data)
		if err != nil {
			slog.Error("imageproxy: transform failed", "err", err)
			httpx.Error(w, http.StatusUnprocessableEntity, "image transform failed: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", outCT)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		slog.Warn("imageproxy: stream aborted", "url", rawURL, "err", err)
	}
}

// fetchUpstream follows redirects (re-checking each hop for private IPs) and
// validates the final response is an image.
func (d Deps) fetchUpstream(ctx context.Context, u *url.URL, sendUA bool) (*http.Response, string, *fetchError) {
	redirects := ssrf.MaxRedirects
	for {
		if ferr := ssrf.AssertNotInternal(ctx, u); ferr != nil {
			return nil, "", ferr
		}
		headers := http.Header{}
		if sendUA {
			headers.Set("User-Agent", upstreamUserAgent)
		}
		resp, ferr := ssrf.SendRequest(ctx, d.Client, u, headers)
		if ferr != nil {
			return nil, "", ferr
		}
		if ssrf.IsRedirect(resp.StatusCode) {
			if redirects == 0 {
				resp.Body.Close()
				return nil, "", errf(ssrf.ErrUpstreamRedirect,
					"exceeded maximum of %d redirects", ssrf.MaxRedirects)
			}
			next, ferr := ssrf.RedirectTarget(u, resp)
			resp.Body.Close()
			if ferr != nil {
				return nil, "", ferr
			}
			slog.Debug("imageproxy: following redirect", "from", u, "to", next)
			redirects--
			u = next
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			status := resp.StatusCode
			resp.Body.Close()
			slog.Warn("imageproxy: upstream non-success", "url", u, "status", status, "with_user_agent", sendUA)
			return nil, "", &fetchError{Kind: ssrf.ErrUpstreamStatus, StatusCode: status,
				Message: fmt.Sprintf("upstream returned status %d", status)}
		}
		ct := ssrf.ContentTypeOf(resp)
		if !isAllowedContentType(ct) {
			resp.Body.Close()
			return nil, "", errf(ssrf.ErrUnexpectedContentType,
				"upstream content-type is not an image: %s", ct)
		}
		return resp, ct, nil
	}
}

func isAllowedContentType(ct string) bool {
	media := ct
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		media = ct[:i]
	}
	media = strings.ToLower(strings.TrimSpace(media))
	return strings.HasPrefix(media, "image/") || media == "application/octet-stream"
}

// --- optional transform (resize/re-encode) ---

type transformParams struct {
	w, h   int
	format string
}

func (t *transformParams) parse(q url.Values) {
	t.w, _ = strconv.Atoi(q.Get("w"))
	t.h, _ = strconv.Atoi(q.Get("h"))
	t.format = q.Get("format")
	if t.format == "jpg" {
		t.format = "jpeg"
	}
}

func (t *transformParams) enabled() bool {
	return t.w > 0 || t.h > 0 || t.format != ""
}

// apply decodes, fits within (w,h) preserving aspect, and re-encodes.
func (t *transformParams) apply(data []byte) ([]byte, string, error) {
	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}
	if t.w > 0 && t.h > 0 {
		img = imaging.Fit(img, t.w, t.h, imaging.Lanczos)
	} else if t.w > 0 || t.h > 0 {
		// A zero dimension preserves aspect ratio.
		img = imaging.Resize(img, t.w, t.h, imaging.Lanczos)
	}
	var buf bytes.Buffer
	var ct string
	switch t.format {
	case "", "jpeg":
		err = imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(90))
		ct = "image/jpeg"
	case "png":
		err = imaging.Encode(&buf, img, imaging.PNG)
		ct = "image/png"
	case "gif":
		err = imaging.Encode(&buf, img, imaging.GIF)
		ct = "image/gif"
	case "bmp":
		err = imaging.Encode(&buf, img, imaging.BMP)
		ct = "image/bmp"
	case "tiff":
		err = imaging.Encode(&buf, img, imaging.TIFF)
		ct = "image/tiff"
	default:
		return nil, "", fmt.Errorf("unsupported output format %q (supported: jpeg, png, gif, bmp, tiff)", t.format)
	}
	if err != nil {
		return nil, "", fmt.Errorf("encode %s: %w", t.format, err)
	}
	return buf.Bytes(), ct, nil
}

// --- error plumbing ---

// fetchError aliases the ssrf FetchError for local helpers.
type fetchError = ssrf.FetchError

func errf(k ssrf.FetchErrorKind, format string, args ...any) *fetchError {
	return &ssrf.FetchError{Kind: k, Message: fmt.Sprintf(format, args...)}
}

// isRetryableWithoutUA mirrors ProxyError::is_retryable_without_ua — a
// non-2xx status or non-image challenge page is worth one retry with no UA.
func isRetryableWithoutUA(e *fetchError) bool {
	return e.Kind == ssrf.ErrUpstreamStatus || e.Kind == ssrf.ErrUnexpectedContentType
}

func writeErr(w http.ResponseWriter, e *fetchError) {
	httpx.Error(w, e.HTTPStatus(), e.Error())
}
