package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/macro-inc/macro/internal/api/contacts"
	"github.com/macro-inc/macro/internal/api/convert"
	"github.com/macro-inc/macro/internal/api/imageproxy"
	"github.com/macro-inc/macro/internal/api/unfurl"
	"github.com/macro-inc/macro/pkg/config"
)

// testDeps returns serviceDeps wired with nil stores — enough for route
// registration; handlers that touch nil deps will fail on request, which is
// fine since we only assert routing/auth behavior below.
func testDeps() *serviceDeps {
	d := &serviceDeps{cfg: Config{Config: config.Config{}}}
	d.contactsSvc = &contacts.Service{Repo: nil, Notifier: contacts.NoopNotifier{}}
	return d
}

func TestRouterRoutes(t *testing.T) {
	r := testDeps().router()

	cases := []struct {
		method, path string
		want         int
	}{
		// health
		{"GET", "/health", 200},
		{"GET", "/api/health", 200},
		{"GET", "/image-proxy/health", 200},
		{"GET", "/unfurl/health", 200},
		{"GET", "/contacts/health", 200},
		{"GET", "/convert/health", 200},

		// static file service (auth required → 401 without credentials)
		{"GET", "/api/file/metadata/abc", 401},
		{"GET", "/internal/file/metadata/abc", 401},
		{"PUT", "/api/file", 401},
		{"DELETE", "/api/file/abc", 401},
		{"POST", "/api/file/bulk-delete", 401},

		// image proxy: root + prefixed, auth required
		{"GET", "/proxy?url=https://example.com", 401},
		{"GET", "/image-proxy/proxy?url=https://example.com", 401},

		// unfurl: no auth required → handler runs, bogus url → 500 null body
		{"GET", "/unfurl?url=::bad::", 500},
		{"GET", "/unfurl/unfurl?url=::bad::", 500},
		{"GET", "/unfurl/proxy?url=::bad::", 400},
		// unfurl's generic proxy does not own root /proxy (image proxy does).
		{"GET", "/proxy?url=::bad::", 401},

		// contacts: auth required
		{"GET", "/contacts", 401},
		{"POST", "/contacts", 401},
		{"GET", "/contacts/contacts", 401},

		// convert: internal-only
		{"POST", "/internal/convert", 401},
		{"POST", "/convert/internal/convert", 401},
		{"POST", "/internal/backfill/docx", 401},

		// nothing else leaks
		{"GET", "/does-not-exist", 404},
	}

	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s: got %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}

func TestRouterBulkUnfurl(t *testing.T) {
	r := testDeps().router()
	for _, path := range []string{"/unfurl/bulk", "/unfurl/unfurl/bulk"} {
		req := httptest.NewRequest("POST", path,
			strings.NewReader(`{"url_list":[]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("POST %s: got %d, want 200", path, rec.Code)
		}
	}
}

func TestRouterInternalKey(t *testing.T) {
	d := testDeps()
	d.cfg.InternalAPIKey = "test-key"
	d.contactsSvc = &contacts.Service{
		Repo:     nil,
		Notifier: contacts.NoopNotifier{},
	}
	r := d.router()

	req := httptest.NewRequest("GET", "/api/file/metadata/abc", nil)
	req.Header.Set("x-internal-auth-key", "test-key")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// Passes auth; fails on the nil metadata store — anything but 401 proves
	// the internal key path is wired.
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("internal key auth rejected")
	}
}

var _ = convert.Deps{}
var _ = imageproxy.Deps{}
var _ = unfurl.Deps{}
