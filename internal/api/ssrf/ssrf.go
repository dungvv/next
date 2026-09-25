// Package ssrf ports the Rust `http_safety` SSRF guard used by
// unfurl_service and image_proxy_service.
//
// Two-layer mitigation, matching the Rust implementation:
//  1. Preflight: AssertNotInternal resolves the host and rejects the request
//     if ANY resolved address is private/internal.
//  2. Connect-time: the http.Transport dialer re-resolves the host and drops
//     private addresses from the candidate set, erroring when nothing public
//     remains. This closes the DNS-rebinding TOCTOU window between the
//     preflight check and the actual connect.
//
// Automatic redirect following is disabled; callers must run the manual loop
// (FetchFollowRedirects) so every hop is re-checked.
package ssrf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// MaxRedirects is the maximum number of redirects followed per fetch.
const MaxRedirects = 5

// FetchError mirrors the Rust FetchError taxonomy; HTTPStatus maps each kind
// to the status the Rust IntoResponse impl returned.
type FetchError struct {
	Kind       FetchErrorKind
	Message    string
	StatusCode int // upstream status, for KindUpstreamStatus
}

type FetchErrorKind int

const (
	ErrInvalidURL FetchErrorKind = iota
	ErrInvalidScheme
	ErrMissingHost
	ErrDNSLookup
	ErrPrivateIP
	ErrUpstreamTimeout
	ErrUpstreamConnect
	ErrUpstreamRedirect
	ErrUpstreamNetwork
	ErrUpstreamStatus
	ErrUnexpectedContentType
	ErrResponseTooLarge
	ErrResponseRead
)

func (e *FetchError) Error() string { return e.Message }

// HTTPStatus maps the error kind to the status code the Rust services return.
func (e *FetchError) HTTPStatus() int {
	switch e.Kind {
	case ErrInvalidURL, ErrInvalidScheme, ErrMissingHost, ErrDNSLookup:
		return http.StatusBadRequest
	case ErrPrivateIP:
		return http.StatusForbidden
	case ErrUpstreamTimeout:
		return http.StatusGatewayTimeout
	case ErrUpstreamConnect, ErrUpstreamRedirect, ErrUpstreamNetwork,
		ErrUpstreamStatus, ErrResponseRead:
		return http.StatusBadGateway
	case ErrUnexpectedContentType:
		return http.StatusUnsupportedMediaType
	case ErrResponseTooLarge:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func errf(kind FetchErrorKind, format string, args ...any) *FetchError {
	return &FetchError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// IsPrivateIP reports whether ip is loopback/private/link-local/unspecified/
// broadcast — the same set the Rust is_private_ip rejects.
func IsPrivateIP(ip netip.Addr) bool {
	if ip.Is4In6() {
		ip = netip.AddrFrom4(ip.As4())
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		(ip.Is4() && ip.As4() == [4]byte{255, 255, 255, 255})
}

// ValidateURL parses rawURL, requiring http/https and dropping the fragment.
func ValidateURL(rawURL string) (*url.URL, *FetchError) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errf(ErrInvalidURL, "invalid URL: %s", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errf(ErrInvalidScheme, "only http/https URLs are allowed")
	}
	u.Fragment = ""
	return u, nil
}

// AssertNotInternal resolves url's host and fails when any resolved IP is
// private/internal.
func AssertNotInternal(ctx context.Context, u *url.URL) *FetchError {
	host := u.Hostname()
	if host == "" {
		return errf(ErrMissingHost, "missing host")
	}
	// Numeric hosts skip DNS.
	if ip, err := netip.ParseAddr(host); err == nil {
		if IsPrivateIP(ip) {
			return errf(ErrPrivateIP, "requests to private/internal IPs are not allowed")
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return errf(ErrDNSLookup, "DNS lookup failed: %s", err)
	}
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		if IsPrivateIP(ip.Unmap()) {
			slog.Warn("ssrf: blocked request to private/internal IP", "host", host, "ip", ip)
			return errf(ErrPrivateIP, "requests to private/internal IPs are not allowed")
		}
	}
	return nil
}

// safeDialContext resolves host, drops private/internal IPs from the
// candidates, and dials the first public one. Mirrors
// PrivateIpFilteringResolver.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var candidates []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		candidates = []netip.Addr{ip.Unmap()}
	} else {
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, a := range resolved {
			if ip, ok := netip.AddrFromSlice(a.IP); ok {
				candidates = append(candidates, ip.Unmap())
			}
		}
	}
	var dialer net.Dialer
	var lastErr error
	anyPublic := false
	for _, ip := range candidates {
		if IsPrivateIP(ip) {
			slog.Warn("ssrf: dropped private/internal IP from connection candidates",
				"host", host, "ip", ip)
			continue
		}
		anyPublic = true
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}
	if !anyPublic {
		return nil, fmt.Errorf("all resolved IPs for %s are private/internal; refusing to connect", host)
	}
	return nil, fmt.Errorf("connect %s: %w", host, lastErr)
}

// NewClient builds an http.Client with redirects disabled and the
// private-IP-filtering dialer. This is the Go equivalent of
// SsrfSafeHttpClient::new / build_http_client.
func NewClient(requestTimeout, connectTimeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext:           safeDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: requestTimeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// SendRequest issues a GET and classifies transport errors like reqwest's
// is_timeout / is_connect / is_redirect / other split.
func SendRequest(ctx context.Context, client *http.Client, u *url.URL, headers http.Header) (*http.Response, *FetchError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errf(ErrInvalidURL, "invalid URL: %s", err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, classify(err)
	}
	return resp, nil
}

func classify(err error) *FetchError {
	if errors.Is(err, context.DeadlineExceeded) {
		return errf(ErrUpstreamTimeout, "timeout fetching upstream: %s", err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errf(ErrUpstreamTimeout, "timeout fetching upstream: %s", err)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errf(ErrDNSLookup, "DNS lookup failed: %s", dnsErr)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return errf(ErrUpstreamConnect, "connection error fetching upstream: %s", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "private/internal") {
		return errf(ErrPrivateIP, "requests to private/internal IPs are not allowed")
	}
	return errf(ErrUpstreamNetwork, "network error fetching upstream: %s", err)
}

// IsRedirect reports whether status is a 3xx redirect the manual loop should
// follow.
func IsRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
		http.StatusMultipleChoices, http.StatusNotModified:
		return true
	}
	return false
}

// RedirectTarget resolves the response's Location header against current and
// applies the same scheme/fragment hygiene as ValidateURL. Callers must run
// AssertNotInternal on the result before following.
func RedirectTarget(current *url.URL, resp *http.Response) (*url.URL, *FetchError) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil, errf(ErrUpstreamRedirect, "redirect response missing Location header")
	}
	next, err := current.Parse(loc)
	if err != nil {
		return nil, errf(ErrUpstreamRedirect, "could not parse redirect target %s: %s", loc, err)
	}
	if next.Scheme != "http" && next.Scheme != "https" {
		return nil, errf(ErrInvalidScheme, "only http/https URLs are allowed")
	}
	next.Fragment = ""
	return next, nil
}

// ContentTypeOf extracts the upstream Content-Type, defaulting to
// application/octet-stream.
func ContentTypeOf(resp *http.Response) string {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// CheckContentLength enforces a max size against the declared Content-Length.
func CheckContentLength(resp *http.Response, max int64, originalURL string) *FetchError {
	if resp.ContentLength > max && resp.ContentLength >= 0 {
		slog.Info("ssrf: upstream content length", "content_length", resp.ContentLength, "url", originalURL)
		return errf(ErrResponseTooLarge,
			"response exceeds max size of %d bytes (Content-Length: %d)", max, resp.ContentLength)
	}
	return nil
}

// MaxBytesBody returns a body wrapper that fails reads once more than max
// bytes have been produced — the streaming equivalent of apply_size_limit.
func MaxBytesBody(resp *http.Response, max int64, url string) *LimitedBody {
	return &LimitedBody{body: resp.Body, max: max, url: url}
}

// LimitedBody wraps an upstream body and errors once more than max bytes
// have been read — the streaming equivalent of apply_size_limit.
type LimitedBody struct {
	body io.ReadCloser
	max  int64
	n    int64
	url  string
	done bool
}

func (b *LimitedBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	if b.n >= b.max {
		// Probe for a single extra byte: EOF means the body fit exactly.
		var one [1]byte
		m, err := b.body.Read(one[:])
		if m > 0 {
			slog.Warn("ssrf: response exceeded max size during streaming",
				"max", b.max, "url", b.url)
			return 0, fmt.Errorf("response exceeded max size of %d bytes during streaming", b.max)
		}
		b.done = true
		return 0, err
	}
	if int64(len(p)) > b.max-b.n {
		p = p[:b.max-b.n]
	}
	n, err := b.body.Read(p)
	b.n += int64(n)
	return n, err
}

func (b *LimitedBody) Close() error { return b.body.Close() }
