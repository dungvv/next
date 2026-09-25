// Package middleware provides the authentication middleware for the
// authentication service — the Go port of macro_authorization's
// MacroAuthorizationExtractor (UserOrInternal / InternalOnly / UserOnly) and
// the token extraction middleware used by the refresh/session endpoints.
//
// RequireUser validates a real service-issued JWT (or the shared internal
// API key) and resolves the caller to a macro user id. Other services can
// adopt it to stop trusting the raw x-user-id header once they have a
// pkg/identity Validator wired up.
package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/macro-inc/macro/pkg/identity"
)

// HeaderInternalAPIKey mirrors INTERNAL_API_KEY_HEADER (x-internal-auth-key).
const HeaderInternalAPIKey = "x-internal-auth-key"

// HeaderUserID is the trusted acting-user header for internal callers.
const HeaderUserID = "x-user-id"

// InternalUserID mirrors macro_auth InternalUserID / MACRO_INTERNAL_USER_ID.
const InternalUserID = "macro|INTERNAL@macro.com"

// User is the authenticated caller — the Go equivalent of the resolved
// MacroAuthorizationExtractor user (UserContext + macro_user_id).
type User struct {
	// UserID is the macro user id "macro|<email>" (User.id in macrodb).
	// For internal callers it is InternalUserID unless they forward
	// x-user-id.
	UserID string
	// ProviderUserID mirrors UserContext.fusion_user_id: the macro_user
	// table UUID (root_macro_id) when present, else the provider subject.
	ProviderUserID string
	// Email is the caller's email from the access token (empty for
	// internal callers and macro-api-tokens).
	Email string
	// OrganizationID is the user's org, when any.
	OrganizationID *int64
	// Internal is true when the request used the internal API key.
	Internal bool
}

type ctxKey struct{}

// FromContext returns the authenticated User, or false.
func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// RequireUser authenticates every request: a valid internal API key produces
// an internal User (optionally acting as x-user-id); otherwise the access
// token from the Authorization: Bearer header or the access-token cookie is
// validated. Anything else gets 401.
//
// This replaces the trusted-x-user-id Middleware in internal/api/auth for
// routes owned by this service.
func RequireUser(v *identity.Validator, cookies *Cookies, internalAPIKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := authenticate(r, v, cookies, internalAPIKey)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
		})
	}
}

// RequireInternal is InternalOnly: the request must carry the internal key.
func RequireInternal(internalAPIKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !validInternalKey(r, internalAPIKey) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			u := User{Internal: true, UserID: InternalUserID}
			if fwd := r.Header.Get(HeaderUserID); fwd != "" {
				u.UserID = fwd
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
		})
	}
}

func validInternalKey(r *http.Request, internalAPIKey string) bool {
	if internalAPIKey == "" {
		return false
	}
	key := r.Header.Get(HeaderInternalAPIKey)
	return key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(internalAPIKey)) == 1
}

// AccessToken reads the caller's access token from Bearer header or cookie.
func AccessToken(r *http.Request, cookies *Cookies) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if cookies != nil {
		if c, err := r.Cookie(cookies.AccessTokenName()); err == nil {
			return c.Value
		}
	}
	return ""
}

// RefreshToken reads the refresh token from the x-macro-refresh-token header
// or the refresh cookie.
func RefreshToken(r *http.Request, cookies *Cookies) string {
	if h := r.Header.Get(HeaderRefreshToken); h != "" {
		return h
	}
	if cookies != nil {
		if c, err := r.Cookie(cookies.RefreshTokenName()); err == nil {
			return c.Value
		}
	}
	return ""
}

// authenticate resolves a User from either credential source.
func authenticate(r *http.Request, v *identity.Validator, cookies *Cookies, internalAPIKey string) (User, bool) {
	if validInternalKey(r, internalAPIKey) {
		u := User{Internal: true, UserID: InternalUserID}
		if fwd := r.Header.Get(HeaderUserID); fwd != "" {
			u.UserID = fwd
		}
		return u, true
	}
	raw := AccessToken(r, cookies)
	if raw == "" {
		return User{}, false
	}
	id, err := v.ValidateToken(raw)
	if err != nil {
		return User{}, false
	}
	if id.UserID == "" {
		return User{}, false
	}
	return User{
		UserID:         id.UserID,
		ProviderUserID: id.ProviderUserID,
		Email:          id.Email,
		OrganizationID: id.OrganizationID,
	}, true
}
