package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubVerifier resolves any token equal to "good" to a fixed user.
type stubVerifier struct{ uid string }

func (v stubVerifier) Verify(_ context.Context, token string) (string, error) {
	if token == "good" {
		return v.uid, nil
	}
	return "", errors.New("bad token")
}

// echo returns a handler that echoes the resolved caller's UserID, or 418
// when no Caller is present.
func echo() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := FromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("x-internal", map[bool]string{true: "1", false: "0"}[c.Internal])
		_, _ = w.Write([]byte(c.UserID))
	})
}

func TestMiddlewareRejectsBareUserID(t *testing.T) {
	h := MiddlewareWithVerifier("key", stubVerifier{"macro|u@x.com"}, false)(echo())
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderUserID, "macro|attacker@x.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bare x-user-id accepted: got %d", rec.Code)
	}
}

func TestMiddlewareInternalKey(t *testing.T) {
	h := MiddlewareWithVerifier("key", stubVerifier{"macro|u@x.com"}, false)(echo())

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderInternalAPIKey, "key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != InternalUserID || rec.Header().Get("x-internal") != "1" {
		t.Fatalf("internal key rejected: %d %q", rec.Code, rec.Body.String())
	}

	// Internal callers may assert an acting user.
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderInternalAPIKey, "key")
	req.Header.Set(HeaderUserID, "macro|other@x.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "macro|other@x.com" || rec.Header().Get("x-internal") != "1" {
		t.Fatalf("internal x-user-id forward failed: %q", rec.Body.String())
	}
}

func TestMiddlewareBearerJWT(t *testing.T) {
	h := MiddlewareWithVerifier("key", stubVerifier{"macro|u@x.com"}, false)(echo())

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "macro|u@x.com" || rec.Header().Get("x-internal") != "0" {
		t.Fatalf("bearer token rejected: %d %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer bad")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token accepted: %d", rec.Code)
	}
}

func TestMiddlewareCookieToken(t *testing.T) {
	h := MiddlewareWithVerifier("key", stubVerifier{"macro|u@x.com"}, false)(echo())
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "macro-access-token", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "macro|u@x.com" {
		t.Fatalf("cookie token rejected: %d", rec.Code)
	}
}

func TestMiddlewareInsecureHatch(t *testing.T) {
	h := MiddlewareWithVerifier("key", nil, true)(echo())
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderUserID, "macro|dev@x.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "macro|dev@x.com" {
		t.Fatalf("insecure x-user-id rejected: %d", rec.Code)
	}
}

func TestInternalMiddleware(t *testing.T) {
	h := InternalMiddleware("key")(echo())

	// User JWT must not satisfy InternalOnly.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("internal-only accepted user token: %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderInternalAPIKey, "key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("x-internal") != "1" {
		t.Fatalf("internal key rejected: %d", rec.Code)
	}
}

func TestOptionalMiddleware(t *testing.T) {
	h := OptionalMiddlewareWithVerifier("key", stubVerifier{"macro|u@x.com"}, false)(echo())

	// No credentials → handler runs without a Caller.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("optional middleware blocked anonymous: %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "macro|u@x.com" {
		t.Fatalf("optional middleware dropped caller: %q", rec.Body.String())
	}
}
