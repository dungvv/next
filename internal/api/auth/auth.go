// Package auth provides the caller-identity surface the Rust services got
// from macro_authorization's MacroAuthorizationExtractor.
//
// Two caller kinds exist:
//   - internal: authenticated by the shared `x-internal-auth-key` header
//     (constant-time compared against INTERNAL_API_KEY); such callers may
//     assert an acting user via `x-user-id`
//   - user: a macro user id ("macro|<email>") resolved from a verified JWT —
//     either a service-issued session/macro-api token (pkg/identity) or a
//     provider-issued token checked against a JWKS endpoint
//
// The `x-user-id` header is NEVER trusted on its own: it is only honored for
// internal-key callers, or when INSECURE_AUTH=true (local development only,
// logged loudly at startup) preserves the pre-JWT port behavior.
package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/macro-inc/macro/pkg/identity"
)

// InternalUserID mirrors MACRO_INTERNAL_USER_ID in static_file_service.
const InternalUserID = "macro|INTERNAL@macro.com"

// HeaderInternalAPIKey carries the shared internal service key.
const HeaderInternalAPIKey = "x-internal-auth-key"

// HeaderUserID carries the acting macro user id. Trusted ONLY for internal
// callers (or under INSECURE_AUTH); a bare header from a user request is
// ignored.
const HeaderUserID = "x-user-id"

// accessTokenCookies are the cookie names an access token may arrive in
// (mirrors middleware/cookies.go environment prefixes).
var accessTokenCookies = []string{
	"macro-access-token",
	"dev-macro-access-token",
	"local-macro-access-token",
}

// Caller is the authenticated identity of a request.
type Caller struct {
	// UserID is the macro user id (e.g. "macro|user@example.com"). For
	// internal callers it is InternalUserID unless the caller forwarded an
	// acting user via HeaderUserID.
	UserID string
	// Internal is true when the request authenticated with the internal key.
	Internal bool
}

type ctxKey struct{}

// FromContext returns the authenticated Caller, or false.
func FromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(ctxKey{}).(Caller)
	return c, ok
}

// TokenVerifier validates a bearer token and resolves the caller's macro
// user id.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (string, error)
}

// VerifierFunc adapts a function to TokenVerifier.
type VerifierFunc func(ctx context.Context, token string) (string, error)

// Verify implements TokenVerifier.
func (f VerifierFunc) Verify(ctx context.Context, token string) (string, error) {
	return f(ctx, token)
}

// IdentityVerifier adapts pkg/identity.Validator: it verifies service-issued
// session access tokens (HS256/RS256) and macro-api-tokens (kid="macro").
func IdentityVerifier(v *identity.Validator) TokenVerifier {
	return VerifierFunc(func(_ context.Context, raw string) (string, error) {
		id, err := v.ValidateToken(raw)
		if err != nil {
			return "", err
		}
		if id.UserID == "" {
			return "", fmt.Errorf("auth: token has no macro user id")
		}
		return id.UserID, nil
	})
}

// jwksVerifier validates provider-issued JWTs against a remote JWKS endpoint
// (JWT_JWKS_URL / GATEWAY_JWKS_URL — e.g. Casdoor's /api/certs). The key set
// is fetched lazily and cached/refreshed by the oidc RemoteKeySet.
type jwksVerifier struct {
	jwksURL  string
	issuer   string // optional exact iss check; empty skips it
	audience string // optional aud check; empty skips it
	once     sync.Once
	verifier *oidc.IDTokenVerifier
}

// NewJWKSVerifier builds a TokenVerifier over a remote JWKS URL. issuer and
// audience may be empty to skip those checks.
func NewJWKSVerifier(jwksURL, issuer, audience string) TokenVerifier {
	return &jwksVerifier{jwksURL: jwksURL, issuer: issuer, audience: audience}
}

func (v *jwksVerifier) lazy(ctx context.Context) *oidc.IDTokenVerifier {
	v.once.Do(func() {
		cfg := &oidc.Config{SkipIssuerCheck: v.issuer == ""}
		if v.audience == "" {
			cfg.SkipClientIDCheck = true
		} else {
			cfg.ClientID = v.audience
		}
		v.verifier = oidc.NewVerifier(v.issuer, oidc.NewRemoteKeySet(ctx, v.jwksURL), cfg)
	})
	return v.verifier
}

// Verify validates the token signature/expiry (plus iss/aud when configured)
// and maps claims onto a macro user id.
func (v *jwksVerifier) Verify(ctx context.Context, raw string) (string, error) {
	tok, err := v.lazy(ctx).Verify(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("auth: invalid bearer token: %w", err)
	}
	var claims struct {
		Sub         string `json:"sub"`
		Email       string `json:"email"`
		MacroUserID string `json:"macro_user_id"`
		UserID      string `json:"user_id"`
		UID         string `json:"uid"`
	}
	if err := tok.Claims(&claims); err != nil {
		return "", fmt.Errorf("auth: decode token claims: %w", err)
	}
	switch {
	case claims.MacroUserID != "":
		return claims.MacroUserID, nil
	case claims.Email != "":
		return "macro|" + claims.Email, nil
	case claims.UserID != "":
		return claims.UserID, nil
	case claims.UID != "":
		return claims.UID, nil
	case claims.Sub != "":
		return claims.Sub, nil
	default:
		return "", fmt.Errorf("auth: token has no user id claim")
	}
}

// multiVerifier tries each verifier in order (e.g. provider JWKS tokens,
// then service-issued session tokens).
type multiVerifier []TokenVerifier

func (m multiVerifier) Verify(ctx context.Context, token string) (string, error) {
	var lastErr error
	for _, v := range m {
		uid, err := v.Verify(ctx, token)
		if err == nil && uid != "" {
			return uid, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("auth: no token verifier configured")
	}
	return "", lastErr
}

// envSettings is the process-level auth configuration resolved from the
// environment once at startup.
type envSettings struct {
	verifier TokenVerifier
	insecure bool
}

// envAuth builds the default verifier chain + dev escape hatch from env:
//
//	INSECURE_AUTH=true                      → trust x-user-id (dev only)
//	JWT_JWKS_URL (or GATEWAY_JWKS_URL)      → JWKS verifier for provider JWTs
//	  (+ optional JWT_JWKS_ISSUER / JWT_JWKS_AUDIENCE)
//	JWT_SECRET_KEY or JWT_PRIVATE_KEY       → service-issued session tokens
//	  (via pkg/identity; also verifies macro-api-tokens)
var envAuth = sync.OnceValue(func() envSettings {
	var s envSettings
	var verifiers []TokenVerifier

	if jwksURL := firstEnv("JWT_JWKS_URL", "GATEWAY_JWKS_URL"); jwksURL != "" {
		verifiers = append(verifiers, NewJWKSVerifier(jwksURL,
			os.Getenv("JWT_JWKS_ISSUER"), os.Getenv("JWT_JWKS_AUDIENCE")))
	}
	// Session-token verification needs signing-key material; without it an
	// HS256 validator would run with an empty key (forgeable), so it is
	// skipped entirely.
	if icfg, err := identity.Load(); err == nil &&
		(icfg.SessionJWTSecret != "" || icfg.SessionJWTPrivateKey != "") {
		if v, err := identity.NewValidator(icfg); err == nil {
			verifiers = append(verifiers, IdentityVerifier(v))
		} else {
			slog.Warn("auth: session JWT validator unavailable", "err", err)
		}
	}
	switch len(verifiers) {
	case 0:
	case 1:
		s.verifier = verifiers[0]
	default:
		s.verifier = multiVerifier(verifiers)
	}

	s.insecure = insecureAuthEnabled()
	if s.insecure {
		slog.Warn("auth: INSECURE_AUTH=true — any request can assert identity via the x-user-id header with NO credential verification; this is a dev-only escape hatch and must NEVER be set in production")
	}
	if s.verifier == nil && !s.insecure {
		slog.Warn("auth: no JWT verifier configured (set JWT_JWKS_URL/GATEWAY_JWKS_URL or JWT_SECRET_KEY/JWT_PRIVATE_KEY); only x-internal-auth-key requests will authenticate")
	}
	return s
})

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func insecureAuthEnabled() bool {
	v := strings.TrimSpace(os.Getenv("INSECURE_AUTH"))
	return strings.EqualFold(v, "true") || v == "1"
}

// Middleware authenticates requests: a valid internal key produces an
// internal Caller, a verified bearer/cookie JWT produces a user Caller,
// anything else gets 401. Verifier and INSECURE_AUTH are resolved from the
// environment (see envAuth); use MiddlewareWithVerifier to inject them.
func Middleware(internalAPIKey string) func(http.Handler) http.Handler {
	s := envAuth()
	return MiddlewareWithVerifier(internalAPIKey, s.verifier, s.insecure)
}

// MiddlewareWithVerifier is Middleware with an explicit verifier and
// insecure flag (tests and non-env wiring).
func MiddlewareWithVerifier(internalAPIKey string, verifier TokenVerifier, insecure bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller, ok := authenticate(r, internalAPIKey, verifier, insecure)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, caller)))
		})
	}
}

// OptionalMiddleware populates the Caller when credentials are present but
// does not reject unauthenticated requests (mirrors
// OptionalMacroAuthorizationExtractor for public link-shared resources).
func OptionalMiddleware(internalAPIKey string) func(http.Handler) http.Handler {
	s := envAuth()
	return OptionalMiddlewareWithVerifier(internalAPIKey, s.verifier, s.insecure)
}

// OptionalMiddlewareWithVerifier is OptionalMiddleware with explicit deps.
func OptionalMiddlewareWithVerifier(internalAPIKey string, verifier TokenVerifier, insecure bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if caller, ok := authenticate(r, internalAPIKey, verifier, insecure); ok {
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, caller))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// InternalMiddleware is InternalOnly: the request must carry the internal
// API key (constant-time compared). User JWTs are not accepted — it guards
// the /internal service-to-service surface where the Rust stack used
// MacroAuthorizationExtractor<_, InternalOnly>.
func InternalMiddleware(internalAPIKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !validInternalKey(r, internalAPIKey) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			uid := r.Header.Get(HeaderUserID)
			if uid == "" {
				uid = InternalUserID
			}
			caller := Caller{UserID: uid, Internal: true}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, caller)))
		})
	}
}

// RequireInternal writes 401 unless the caller authenticated internally.
// Handlers use it to mirror MacroAuthorizationExtractor<_, InternalOnly>.
func RequireInternal(w http.ResponseWriter, c Caller) bool {
	if !c.Internal {
		http.Error(w, "unauthorized: internal only", http.StatusUnauthorized)
		return false
	}
	return true
}

// validInternalKey compares the presented key in constant time.
func validInternalKey(r *http.Request, internalAPIKey string) bool {
	if internalAPIKey == "" {
		return false
	}
	key := r.Header.Get(HeaderInternalAPIKey)
	return key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(internalAPIKey)) == 1
}

// bearerToken extracts an access token from `Authorization: Bearer` or the
// macro-access-token cookie (any environment prefix).
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	for _, name := range accessTokenCookies {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func authenticate(r *http.Request, internalAPIKey string, verifier TokenVerifier, insecure bool) (Caller, bool) {
	if validInternalKey(r, internalAPIKey) {
		uid := r.Header.Get(HeaderUserID)
		if uid == "" {
			uid = InternalUserID
		}
		return Caller{UserID: uid, Internal: true}, true
	}
	if verifier != nil {
		if raw := bearerToken(r); raw != "" {
			if uid, err := verifier.Verify(r.Context(), raw); err == nil && uid != "" {
				return Caller{UserID: uid, Internal: false}, true
			}
		}
	}
	if insecure {
		// Dev escape hatch: trust the acting-user header unauthenticated,
		// preserving the pre-JWT port behavior.
		if uid := r.Header.Get(HeaderUserID); uid != "" {
			return Caller{UserID: uid, Internal: false}, true
		}
	}
	return Caller{}, false
}
