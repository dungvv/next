package authentication

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macro-inc/macro/internal/api/authentication/middleware"
	"github.com/macro-inc/macro/pkg/identity"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

func testDeps(t *testing.T) Deps {
	t.Helper()
	// pgxpool.New is lazy — no connection is made at construction time.
	pool, err := pgxpool.New(context.Background(), "postgres://unused:5432/macro")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	icfg := identity.Config{
		ClientID:           "client",
		SessionJWTSecret:   "test-secret",
		SessionJWTIssuer:   "macro",
		SessionJWTAudience: "client",
		AccessTokenTTL:     time.Hour,
		RefreshTokenTTL:    24 * time.Hour,
	}
	issuer, err := identity.NewIssuer(icfg)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	validator, err := identity.NewValidator(icfg)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return Deps{
		Cfg:       Config{Env: "local", InternalAPIKey: "internal-key"},
		Pool:      pool,
		Q:         macrodb.New(pool),
		Sessions:  issuer,
		Validator: validator,
	}
}

func TestRegisterDoesNotPanic(t *testing.T) {
	d := testDeps(t)
	rt, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Mirror the combined api binary: a host-owned /internal mount that the
	// auth internal routes merge into, the /auth prefix mount, the root
	// mount minus /internal+/health, and a host /health.
	r := chi.NewRouter()
	r.Route("/internal", func(r chi.Router) {
		r.Get("/other", func(w http.ResponseWriter, _ *http.Request) {})
		rt.RegisterInternal(r)
	})
	r.Route("/auth", func(r chi.Router) { rt.Register(r) })
	rt.RegisterNoInternal(r)
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {})

	// A merged internal route must actually serve.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal/get_existing_users", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without internal key, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/auth/health", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /auth/health, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/login/sso", nil)
	r.ServeHTTP(rec, req)
	// Identity is nil → the handler itself answers 503, not a router miss.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 from /login/sso, got %d", rec.Code)
	}
}

func TestRequireUserFlow(t *testing.T) {
	d := testDeps(t)

	org := int64(7)
	access, err := d.Sessions.IssueAccessToken(identity.AccessClaims{
		Email:               "a@b.com",
		FusionUserID:        "prov-1",
		MacroUserID:         "macro|a@b.com",
		MacroOrganizationID: &org,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	var got middleware.User
	handler := middleware.RequireUser(d.Validator, d.cookies, d.Cfg.InternalAPIKey)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, _ = middleware.FromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

	// No credentials → 401.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Bearer token → resolves claims.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got.UserID != "macro|a@b.com" || got.ProviderUserID != "prov-1" {
		t.Fatalf("unexpected user: %+v", got)
	}
	if got.OrganizationID == nil || *got.OrganizationID != 7 {
		t.Fatalf("unexpected org: %+v", got.OrganizationID)
	}

	// Internal key → internal user honoring x-user-id.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(middleware.HeaderInternalAPIKey, "internal-key")
	req.Header.Set(middleware.HeaderUserID, "macro|other@x.com")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got.UserID != "macro|other@x.com" || !got.Internal {
		t.Fatalf("internal auth failed: code=%d user=%+v", rec.Code, got)
	}

	// RootMacroID claim takes precedence as the provider user id.
	root := "00000000-0000-0000-0000-0000000000aa"
	access, _ = d.Sessions.IssueAccessToken(identity.AccessClaims{
		Email: "a@b.com", FusionUserID: "prov-1",
		MacroUserID: "macro|a@b.com", RootMacroID: &root,
	})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	handler.ServeHTTP(rec, req)
	if got.ProviderUserID != root {
		t.Fatalf("root_macro_id not honored: %+v", got)
	}
}

func TestRefreshRoundtrip(t *testing.T) {
	d := testDeps(t)
	refresh, jti, err := d.Sessions.IssueRefreshToken("macro|a@b.com", "a@b.com")
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	if jti == "" {
		t.Fatal("expected non-empty jti")
	}
	claims, err := d.Validator.ValidateRefreshToken(refresh)
	if err != nil {
		t.Fatalf("validate refresh: %v", err)
	}
	if claims.Subject != "macro|a@b.com" {
		t.Fatalf("unexpected subject %q", claims.Subject)
	}
	if claims.ID != jti {
		t.Fatalf("jti mismatch: %q != %q", claims.ID, jti)
	}
	// A refresh token must not pass access validation (missing aud).
	if _, err := d.Validator.ValidateAccessToken(refresh); err == nil {
		t.Fatal("refresh token accepted as access token")
	}
}

func TestRS256SessionRoundtrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	icfg := identity.Config{
		ClientID:             "client",
		SessionJWTPrivateKey: string(priv),
		SessionJWTIssuer:     "macro",
		SessionJWTAudience:   "client",
		AccessTokenTTL:       time.Hour,
		RefreshTokenTTL:      24 * time.Hour,
	}
	issuer, err := identity.NewIssuer(icfg)
	if err != nil {
		t.Fatalf("NewIssuer RS256: %v", err)
	}
	validator, err := identity.NewValidator(icfg)
	if err != nil {
		t.Fatalf("NewValidator RS256: %v", err)
	}
	tok, err := issuer.IssueAccessToken(identity.AccessClaims{
		Email: "a@b.com", MacroUserID: "macro|a@b.com",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	id, err := validator.ValidateToken(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if id.UserID != "macro|a@b.com" {
		t.Fatalf("unexpected user %q", id.UserID)
	}
	// And HS256 issue/validate still works on the default config.
	d := testDeps(t)
	tok, err = d.Sessions.IssueAccessToken(identity.AccessClaims{
		Email: "a@b.com", MacroUserID: "macro|a@b.com",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	id, err = d.Validator.ValidateToken(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if id.UserID != "macro|a@b.com" {
		t.Fatalf("unexpected user %q", id.UserID)
	}
}

func TestAllowedOriginalURL(t *testing.T) {
	cases := []struct {
		raw   string
		extra []string
		want  bool
	}{
		{"https://macro.com/app", nil, true},
		{"https://dev.macro.com/x", nil, true},
		{"http://localhost:3000", nil, true},
		{"tauri://localhost/x", nil, true},
		{"macro://anything", nil, true},
		{"https://evil.com", nil, false},
		{"https://selfhost.example.com", []string{"selfhost.example.com"}, true},
		{"javascript:alert(1)", nil, false},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		if got := isAllowedOriginalURL(u, c.extra); got != c.want {
			t.Errorf("isAllowedOriginalURL(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestShortUUIDRoundtrip(t *testing.T) {
	u := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	short := shortUUIDFromUUID(u)
	back, ok := uuidFromShortUUID(short)
	if !ok || back != u {
		t.Fatalf("roundtrip failed: %q → %v", short, back)
	}
	// Invalid input is rejected.
	if _, ok := uuidFromShortUUID("not-base58!!"); ok {
		t.Fatal("expected rejection")
	}
	if _, ok := uuidFromShortUUID(""); ok {
		t.Fatal("expected rejection of empty")
	}
}
