package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// TokenVerifier validates a bearer token presented on the websocket
// handshake and resolves the macro user id that owns the connection.
//
// The interface is deliberately small so the middleware stays swappable:
// the Rust service used macro_auth JWT validation against FusionAuth; the
// self-host port targets Casdoor-issued JWTs (HMAC secret today, JWKS via
// GATEWAY_JWKS_URL once wired).
type TokenVerifier interface {
	// Verify returns the authenticated user id, or an error.
	Verify(ctx context.Context, token string) (string, error)
}

// newVerifier picks a verifier from config:
// JWKSURL > JWTSecret (HMAC) > insecure (dev only) > fail-closed.
func newVerifier(cfg Config) (TokenVerifier, error) {
	switch {
	case cfg.JWKSURL != "":
		return &jwksVerifier{url: cfg.JWKSURL}, nil
	case cfg.JWTSecret != "":
		return &hmacVerifier{secret: []byte(cfg.JWTSecret)}, nil
	case cfg.InsecureAuth:
		slog.Warn("gateway: GATEWAY_INSECURE_AUTH=1, websocket JWTs are NOT verified — dev only")
		return insecureVerifier{}, nil
	default:
		slog.Error("gateway: no GATEWAY_JWT_SECRET/GATEWAY_JWKS_URL configured; all websocket auth will fail")
		return failVerifier{}, nil
	}
}

// hmacVerifier validates HS256/384/512 tokens signed with a shared secret.
type hmacVerifier struct {
	secret []byte
}

func (v *hmacVerifier) Verify(_ context.Context, token string) (string, error) {
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims,
		func(t *jwt.Token) (any, error) { return v.secret, nil },
		jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}),
	)
	if err != nil {
		return "", fmt.Errorf("jwt parse: %w", err)
	}
	if !parsed.Valid {
		return "", errors.New("jwt invalid")
	}
	return userIDFromClaims(claims)
}

// jwksVerifier is a placeholder for asymmetric (Casdoor RS256/ES256)
// verification. TODO: fetch and cache the JWKS, verify signature + iss/aud.
type jwksVerifier struct {
	url string
}

func (v *jwksVerifier) Verify(context.Context, string) (string, error) {
	// TODO: implement JWKS fetch/cache (e.g. lestrrat-go/jwx or MicahParks/keyfunc).
	return "", fmt.Errorf("jwks verification not implemented yet (url=%s)", v.url)
}

// insecureVerifier reads claims without verifying the signature. Dev only.
type insecureVerifier struct{}

func (insecureVerifier) Verify(_ context.Context, token string) (string, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return "", fmt.Errorf("jwt parse (unverified): %w", err)
	}
	return userIDFromClaims(claims)
}

// failVerifier rejects everything; used when no auth is configured.
type failVerifier struct{}

func (failVerifier) Verify(context.Context, string) (string, error) {
	return "", errors.New("no jwt verifier configured")
}

// userIDFromClaims resolves the macro user id from standard and legacy
// claim names.
func userIDFromClaims(claims jwt.MapClaims) (string, error) {
	for _, key := range []string{"sub", "user_id", "macro_user_id", "uid"} {
		if v, ok := claims[key].(string); ok && v != "" {
			return v, nil
		}
	}
	return "", errors.New("token has no user id claim")
}

// bearerToken extracts the JWT from `Authorization: Bearer …` or the
// `?token=` query parameter (browsers cannot set headers on WS upgrade).
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if strings.HasPrefix(h, "Bearer ") {
			return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
		return strings.TrimSpace(h)
	}
	return r.URL.Query().Get("token")
}
