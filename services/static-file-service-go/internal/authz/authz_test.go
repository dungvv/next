package authz

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testAuthorizer(t *testing.T) *Authorizer {
	t.Helper()
	a, err := New(Config{
		InternalAPIKey:      "secret-key",
		JWTSecret:           "hmac-secret",
		Audience:            "aud",
		Issuer:              "iss",
		MacroAPITokenIssuer: "macro",
		Environment:         "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func signAccessToken(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "fusion"
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	a := FromContext(r.Context())
	if a == nil {
		w.WriteHeader(http.StatusTeapot)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func doReq(t *testing.T, a *Authorizer, r *http.Request) int {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(okHandler)).ServeHTTP(rec, r)
	return rec.Code
}

func TestInternalKey(t *testing.T) {
	a := testAuthorizer(t)
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set(InternalAPIKeyHeader, "secret-key")
	if code := doReq(t, a, r); code != http.StatusNoContent {
		t.Fatalf("want 204 got %d", code)
	}
}

func TestInternalKeyInvalid(t *testing.T) {
	a := testAuthorizer(t)
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set(InternalAPIKeyHeader, "wrong")
	if code := doReq(t, a, r); code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", code)
	}
}

func TestBearerHS256(t *testing.T) {
	a := testAuthorizer(t)
	token := signAccessToken(t, "hmac-secret", jwt.MapClaims{
		"aud": "aud", "iss": "iss",
		"exp":            time.Now().Add(time.Hour).Unix(),
		"fusion_user_id": "fu",
		"macro_user_id":  "macro|user@macro.com",
	})
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if code := doReq(t, a, r); code != http.StatusNoContent {
		t.Fatalf("want 204 got %d", code)
	}
}

func TestBearerExpired(t *testing.T) {
	a := testAuthorizer(t)
	token := signAccessToken(t, "hmac-secret", jwt.MapClaims{
		"aud": "aud", "iss": "iss",
		"exp":           time.Now().Add(-time.Hour).Unix(),
		"macro_user_id": "macro|user@macro.com",
	})
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if code := doReq(t, a, r); code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", code)
	}
}

func TestAmbiguousCredentials(t *testing.T) {
	a := testAuthorizer(t)
	token := signAccessToken(t, "hmac-secret", jwt.MapClaims{
		"aud": "aud", "iss": "iss",
		"exp":           time.Now().Add(time.Hour).Unix(),
		"macro_user_id": "macro|user@macro.com",
	})
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set(InternalAPIKeyHeader, "secret-key")
	r.Header.Set("Authorization", "Bearer "+token)
	if code := doReq(t, a, r); code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", code)
	}
}

func TestNoCredentials(t *testing.T) {
	a := testAuthorizer(t)
	r := httptest.NewRequest("GET", "/x", nil)
	if code := doReq(t, a, r); code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", code)
	}
}

func TestAmbientCookie(t *testing.T) {
	a := testAuthorizer(t)
	token := signAccessToken(t, "hmac-secret", jwt.MapClaims{
		"aud": "aud", "iss": "iss",
		"exp":           time.Now().Add(time.Hour).Unix(),
		"macro_user_id": "macro|user@macro.com",
	})
	r := httptest.NewRequest("GET", "/x", nil)
	r.AddCookie(&http.Cookie{Name: "local-macro-access-token", Value: token})
	if code := doReq(t, a, r); code != http.StatusNoContent {
		t.Fatalf("want 204 got %d", code)
	}
}
