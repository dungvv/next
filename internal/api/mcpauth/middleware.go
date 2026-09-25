package mcpauth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/macro-inc/macro/pkg/identity"
)

// ValidatedCaller is what a validated bearer token asserts.
type ValidatedCaller struct {
	// UserID is the macro user id ("macro|<email>").
	UserID string
	// Token is the raw bearer token (downstream tool calls may forward it).
	Token string
}

// TokenValidator validates an `Authorization: Bearer` token and resolves the
// macro caller. It replaces decode_jwt::handler in the Rust middleware.
type TokenValidator interface {
	ValidateToken(ctx context.Context, rawToken string) (*ValidatedCaller, error)
}

// IdentityTokenValidator validates service-issued tokens (macro session and
// macro-api tokens) through pkg/identity's Validator.
type IdentityTokenValidator struct {
	V *identity.Validator
}

// ValidateToken implements TokenValidator.
func (v IdentityTokenValidator) ValidateToken(_ context.Context, raw string) (*ValidatedCaller, error) {
	id, err := v.V.ValidateToken(raw)
	if err != nil {
		return nil, err
	}
	return &ValidatedCaller{UserID: id.UserID, Token: raw}, nil
}

// MacroUserLookup resolves a provider subject (Casdoor user id) to the macro
// user id ("macro|<email>"). Implemented by the composition root over
// macrodb ("User".macro_user_id → "User".id).
type MacroUserLookup interface {
	LookupMacroUserID(ctx context.Context, providerUserID, email string) (string, error)
}

// CasdoorTokenValidator validates upstream Casdoor access tokens (the
// JWTs the OAuth broker passes through to MCP clients) via OIDC JWKS
// discovery, then maps the provider subject to a macro user id.
//
// TODO(mcp-auth): the FusionAuth tokens carried a macro_user_id claim so no
// lookup was needed; under Casdoor we resolve via MacroUserLookup. Confirm
// the deployed Casdoor mints access tokens with aud=<client id> so the OIDC
// verifier accepts them — otherwise switch to token introspection.
type CasdoorTokenValidator struct {
	Issuer   string // CASDOOR_ENDPOINT (server-to-server issuer/discovery base)
	ClientID string // expected audience
	Lookup   MacroUserLookup

	once     sync.Once
	verifier *oidc.IDTokenVerifier
	err      error
}

func (v *CasdoorTokenValidator) lazyVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.once.Do(func() {
		p, err := oidc.NewProvider(ctx, strings.TrimRight(v.Issuer, "/"))
		if err != nil {
			v.err = fmt.Errorf("mcpauth: oidc discovery: %w", err)
			return
		}
		v.verifier = p.Verifier(&oidc.Config{ClientID: v.ClientID})
	})
	return v.verifier, v.err
}

// ValidateToken implements TokenValidator.
func (v *CasdoorTokenValidator) ValidateToken(ctx context.Context, raw string) (*ValidatedCaller, error) {
	verifier, err := v.lazyVerifier(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("mcpauth: invalid bearer token: %w", err)
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("mcpauth: decode bearer claims: %w", err)
	}
	if v.Lookup == nil {
		return nil, fmt.Errorf("mcpauth: no macro user lookup configured")
	}
	userID, err := v.Lookup.LookupMacroUserID(ctx, claims.Sub, claims.Email)
	if err != nil {
		return nil, err
	}
	return &ValidatedCaller{UserID: userID, Token: raw}, nil
}

// ---------------------------------------------------------------------------
// Bearer middleware (inbound::middleware::validate_bearer)
// ---------------------------------------------------------------------------

type ctxKey struct{ name string }

var (
	callerKey = ctxKey{"caller"}
	tokenKey  = ctxKey{"token"}
)

// CallerFromContext returns the bearer-authenticated caller user id.
func CallerFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(callerKey).(string)
	return v, ok
}

// AccessTokenFromContext returns the raw bearer token (JwtAccessToken in Rust).
func AccessTokenFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(tokenKey).(string)
	return v, ok
}

func absoluteResourceMetadataURL(r *http.Request, metadataPath string) string {
	scheme := r.Header.Get("x-forwarded-proto")
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	authority := r.Host
	if authority == "" {
		authority = "localhost"
	}
	return scheme + "://" + authority + metadataPath
}

func unauthorized(w http.ResponseWriter, r *http.Request, metadataPath string, errorCode string) {
	metadata := absoluteResourceMetadataURL(r, metadataPath)
	var challenge string
	if errorCode != "" {
		challenge = fmt.Sprintf(`Bearer error="%s", resource_metadata="%s"`, errorCode, metadata)
	} else {
		challenge = fmt.Sprintf(`Bearer resource_metadata="%s"`, metadata)
	}
	w.Header().Set("WWW-Authenticate", challenge)
	w.WriteHeader(http.StatusUnauthorized)
}

// BearerMiddleware validates `Authorization: Bearer <token>` and stores the
// caller's macro user id and raw token in the request context. `metadataPath`
// is the public path of the protected-resource metadata document advertised
// in the challenge (e.g. "/mcp/.well-known/oauth-protected-resource").
func BearerMiddleware(v TokenValidator, metadataPath string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			token, found := strings.CutPrefix(auth, "Bearer ")
			if !found || token == "" {
				unauthorized(w, r, metadataPath, "")
				return
			}
			caller, err := v.ValidateToken(r.Context(), token)
			if err != nil || caller.UserID == "" {
				if err != nil {
					slog.Warn("mcpauth: bearer token validation failed", "err", err)
				}
				unauthorized(w, r, metadataPath, "invalid_token")
				return
			}
			ctx := context.WithValue(r.Context(), callerKey, caller.UserID)
			ctx = context.WithValue(ctx, tokenKey, caller.Token)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
