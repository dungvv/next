package syncsvc

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testPermSecret = "perm-secret"

func permToken(t *testing.T, docID, level string) string {
	t.Helper()
	uid := "user-1"
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, authClaims{
		UserID:      &uid,
		DocumentID:  docID,
		AccessLevel: level,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "document_storage_service",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	s, err := tok.SignedString([]byte(testPermSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testServer(cfg syncConfig) *Server {
	return &Server{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestAuthenticate(t *testing.T) {
	srv := testServer(syncConfig{
		InternalAPIKey:      "internal-key",
		PermissionJWTSecret: testPermSecret,
	})

	req := httptest.NewRequest("GET", "/document/doc-1/state", nil)

	// No credentials at all → reject (no insecure hatch).
	if _, err := srv.authenticate(req, "doc-1", syncLevelView); err == nil {
		t.Fatal("unauthenticated request accepted")
	}

	// Wrong internal key → reject.
	r := httptest.NewRequest("GET", "/document/doc-1/state", nil)
	r.Header.Set(headerInternalAuth, "wrong")
	if _, err := srv.authenticate(r, "doc-1", syncLevelView); err == nil {
		t.Fatal("wrong internal key accepted")
	}

	// Correct internal key → admin, any doc.
	r = httptest.NewRequest("GET", "/document/doc-1/state", nil)
	r.Header.Set(headerInternalAuth, "internal-key")
	c, err := srv.authenticate(r, "doc-1", syncLevelAdmin)
	if err != nil || c.level() != syncLevelAdmin {
		t.Fatalf("internal key: %v %+v", err, c)
	}

	// Permission JWT for the right doc at view level passes view, fails edit.
	r = httptest.NewRequest("GET", "/document/doc-1/state", nil)
	r.Header.Set("Authorization", "Bearer "+permToken(t, "doc-1", "view"))
	if _, err := srv.authenticate(r, "doc-1", syncLevelView); err != nil {
		t.Fatalf("view token on view endpoint: %v", err)
	}
	if _, err := srv.authenticate(r, "doc-1", syncLevelEdit); err == nil {
		t.Fatal("view token passed edit-gated endpoint")
	}

	// Permission JWT for a different doc → reject (doc-scoped authz).
	r = httptest.NewRequest("GET", "/document/doc-1/state", nil)
	r.Header.Set("Authorization", "Bearer "+permToken(t, "other-doc", "owner"))
	if _, err := srv.authenticate(r, "doc-1", syncLevelView); err == nil {
		t.Fatal("token for another document accepted")
	}

	// Admin-level permission JWT passes any doc.
	r = httptest.NewRequest("GET", "/document/doc-1/state", nil)
	r.Header.Set("Authorization", "Bearer "+permToken(t, "whatever", "admin"))
	if _, err := srv.authenticate(r, "doc-1", syncLevelAdmin); err != nil {
		t.Fatalf("admin token: %v", err)
	}

	// Garbage token → reject.
	r = httptest.NewRequest("GET", "/document/doc-1/state", nil)
	r.Header.Set("Authorization", "Bearer not-a-jwt")
	if _, err := srv.authenticate(r, "doc-1", syncLevelView); err == nil {
		t.Fatal("garbage token accepted")
	}

	// ws-style ?token= query param works too.
	r = httptest.NewRequest("GET", "/document/doc-1/connect?token="+permToken(t, "doc-1", "edit"), nil)
	c, err = srv.authenticate(r, "doc-1", syncLevelEdit)
	if err != nil || c.userID() != "user-1" {
		t.Fatalf("query token: %v %+v", err, c)
	}

	// Forged signing method must be rejected — jwt.ParseWithClaims enforces
	// HS256 via the keyfunc guard.
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, authClaims{
		DocumentID: "doc-1", AccessLevel: "admin",
	})
	s, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	r = httptest.NewRequest("GET", "/document/doc-1/state", nil)
	r.Header.Set("Authorization", "Bearer "+s)
	if _, err := srv.authenticate(r, "doc-1", syncLevelView); err == nil {
		t.Fatal("alg=none token accepted")
	}
}

func TestAuthenticateInsecureHatch(t *testing.T) {
	srv := testServer(syncConfig{InsecureAuth: true})
	r := httptest.NewRequest("GET", "/document/d/state?user_id=dev-user", nil)
	c, err := srv.authenticate(r, "d", syncLevelView)
	if err != nil || c.userID() != "dev-user" {
		t.Fatalf("insecure mode: %v %+v", err, c)
	}
}

// Every /document/* route must reject a credential-less request when neither
// insecure mode nor keys/tokens are configured.
func TestDocumentRoutesRequireAuth(t *testing.T) {
	srv := testServer(syncConfig{})
	for _, route := range []string{
		"/document/d/connect",
		"/document/d/exists",
		"/document/d/initialize",
		"/document/d/snapshot",
		"/document/d/state",
		"/document/d/update",
		"/document/d/raw",
		"/document/d/active_peers",
		"/document/d/peer/1",
		"/document/d/metadata",
		"/document/d/blame/node-1",
		"/document/d/copy",
		"/document/d/wakeup",
		"/document/d/debug_dump_operations",
	} {
		r := httptest.NewRequest("GET", route, nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s: expected 401, got %d", route, w.Code)
		}
	}
}
