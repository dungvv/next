package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Session-token machinery.
//
// In the Rust stack FusionAuth issued the macro-access-token (HS256, kid set
// by FusionAuth) and authentication_service issued macro-api-tokens (RS256,
// kid="macro"). Here the service issues both itself after the provider code
// exchange: the access token keeps the exact MacroAccessToken claim shape so
// any consumer ported from macro_auth sees identical claims, and the refresh
// token is a second service-signed JWT (type=refresh) — stateless, so refresh
// works without Valkey.

// AccessClaims mirrors macro_auth::middleware::decode_jwt::MacroAccessToken
// field-for-field (claim names unchanged for compatibility).
type AccessClaims struct {
	// Audience of the token (was the FusionAuth application id; now the
	// configured JWT_AUDIENCE / Casdoor client id).
	Audience string `json:"aud"`
	// ExpiresAt is the unix expiry.
	ExpiresAt int64 `json:"exp"`
	// TenantID was the FusionAuth tenant; empty under Casdoor.
	TenantID string `json:"tid"`
	// Issuer of the token (was the FusionAuth domain).
	Issuer string `json:"iss"`
	// Email of the user.
	Email string `json:"email"`
	// FusionUserID keeps the legacy claim name; it now holds the identity
	// provider subject (Casdoor user UUID).
	FusionUserID string `json:"fusion_user_id"`
	// MacroUserID is the macro user id ("macro|<email>" — User.id in macrodb).
	MacroUserID string `json:"macro_user_id"`
	// MacroOrganizationID is set when the user belongs to an org.
	MacroOrganizationID *int64 `json:"macro_organization_id"`
	// RootMacroID is the macro_user table UUID; when absent consumers fall
	// back to FusionUserID (same as Rust).
	RootMacroID *string `json:"root_macro_id"`
}

// jwt.Claims implementation (golang-jwt/v5 interface).
func (c AccessClaims) GetExpirationTime() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.ExpiresAt, 0)), nil
}
func (c AccessClaims) GetIssuedAt() (*jwt.NumericDate, error)  { return nil, nil }
func (c AccessClaims) GetNotBefore() (*jwt.NumericDate, error) { return nil, nil }
func (c AccessClaims) GetIssuer() (string, error)              { return c.Issuer, nil }
func (c AccessClaims) GetSubject() (string, error)             { return c.MacroUserID, nil }
func (c AccessClaims) GetAudience() (jwt.ClaimStrings, error) {
	return jwt.ClaimStrings{c.Audience}, nil
}

// refreshClaims is the internal refresh-token shape. Refresh tokens are only
// ever consumed by this service's /jwt/refresh + /session/login endpoints.
// The jti (RegisteredClaims.ID) is what the RefreshStore rotates/revokes on.
type refreshClaims struct {
	jwt.RegisteredClaims
	Type string `json:"typ"` // always "refresh"
}

// RefreshClaims is what ValidateRefreshToken returns: the macro user id the
// token was issued for plus the token id used for rotation/revocation.
type RefreshClaims struct {
	// Subject is the macro user id ("macro|<email>").
	Subject string
	// ID is the jti. Empty for tokens issued before rotation existed —
	// callers may accept those once (legacy) but cannot revoke them.
	ID string
	// ExpiresAt is the token expiry.
	ExpiresAt time.Time
}

// MacroAPIClaims mirrors macro_auth::macro_api_token::MacroApiToken (RS256,
// kid="macro"), the long-lived token handed to internal callers.
type MacroAPIClaims struct {
	jwt.RegisteredClaims
	FusionUserID        string `json:"fusion_user_id"`
	MacroUserID         string `json:"macro_user_id"`
	MacroOrganizationID *int64 `json:"macro_organization_id"`
}

// Signing key kinds for Issuer.
const (
	algHS256 = "HS256"
	algRS256 = "RS256"
)

// Issuer signs Macro session tokens.
type Issuer struct {
	method     jwt.SigningMethod
	hmacKey    []byte
	rsaKey     *rsa.PrivateKey
	issuer     string
	audience   string
	accessTTL  time.Duration
	refreshTTL time.Duration

	// stateKey is the HMAC key for signed OAuth state envelopes. It is the
	// session HMAC key under HS256; under RS256 it is derived from the RSA
	// private key (never exposed publicly — the public key cannot recompute
	// it, which is the point).
	stateKey []byte
}

// NewIssuer builds an Issuer from Config. RS256 is used when
// SessionJWTPrivateKey is set, otherwise HS256 with SessionJWTSecret.
func NewIssuer(cfg Config) (*Issuer, error) {
	i := &Issuer{
		issuer:     cfg.SessionJWTIssuer,
		audience:   cfg.SessionJWTAudience,
		accessTTL:  cfg.AccessTokenTTL,
		refreshTTL: cfg.RefreshTokenTTL,
	}
	if cfg.SessionJWTPrivateKey != "" {
		key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(cfg.SessionJWTPrivateKey))
		if err != nil {
			return nil, fmt.Errorf("identity: parse JWT_PRIVATE_KEY: %w", err)
		}
		i.method, i.rsaKey = jwt.SigningMethodRS256, key
		sum := sha256.Sum256(x509.MarshalPKCS1PrivateKey(key))
		i.stateKey = sum[:]
		return i, nil
	}
	if cfg.SessionJWTSecret == "" {
		return nil, fmt.Errorf("identity: JWT_SECRET_KEY is required for session tokens")
	}
	i.method, i.hmacKey = jwt.SigningMethodHS256, []byte(cfg.SessionJWTSecret)
	i.stateKey = i.hmacKey
	return i, nil
}

// RefreshTTL exposes the configured refresh-token lifetime — the TTL the
// RefreshStore registers each token id with.
func (i *Issuer) RefreshTTL() time.Duration { return i.refreshTTL }

func (i *Issuer) sign(claims jwt.Claims, kid string) (string, error) {
	tok := jwt.NewWithClaims(i.method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	if i.method == jwt.SigningMethodRS256 {
		return tok.SignedString(i.rsaKey)
	}
	return tok.SignedString(i.hmacKey)
}

// IssueAccessToken signs a macro-access-token for an authenticated user.
// Callers fill the identity fields; issuer/audience/expiry come from config.
func (i *Issuer) IssueAccessToken(c AccessClaims) (string, error) {
	c.Audience = i.audience
	c.Issuer = i.issuer
	c.ExpiresAt = time.Now().Add(i.accessTTL).Unix()
	return i.sign(c, "session")
}

// IssueRefreshToken signs a refresh token bound to a macro user id. The
// returned id is the token's jti — callers register it with the RefreshStore
// so the token can be rotated/revoked.
func (i *Issuer) IssueRefreshToken(macroUserID, email string) (token, id string, err error) {
	jti, err := newTokenID()
	if err != nil {
		return "", "", err
	}
	token, err = i.sign(refreshClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   macroUserID,
			ID:        jti,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(i.refreshTTL)),
		},
		Type: "refresh",
	}, "session")
	if err != nil {
		return "", "", err
	}
	return token, jti, nil
}

// newTokenID mints a random 128-bit token id (hex).
func newTokenID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// TokenPair is what /jwt/refresh and /session/login return (mirrors
// model::response::UserTokensResponse).
type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// Validator verifies service-issued tokens — the Go port of
// decode_jwt::handler (kid=="macro" → macro-api-token RS256, otherwise the
// HS256/RS256 access token) plus refresh-token verification.
type Validator struct {
	method   jwt.SigningMethod
	hmacKey  []byte
	rsaPub   *rsa.PublicKey
	issuer   string
	audience string

	apiTokenPub *rsa.PublicKey
	apiTokenIss string
}

// NewValidator builds a Validator from Config. When the api-token public key
// is unset, macro-api-tokens are rejected (matching a deployment that never
// issued them).
func NewValidator(cfg Config) (*Validator, error) {
	v := &Validator{issuer: cfg.SessionJWTIssuer, audience: cfg.SessionJWTAudience}
	if cfg.SessionJWTPrivateKey != "" {
		priv, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(cfg.SessionJWTPrivateKey))
		if err != nil {
			return nil, fmt.Errorf("identity: parse JWT_PRIVATE_KEY: %w", err)
		}
		v.method, v.rsaPub = jwt.SigningMethodRS256, &priv.PublicKey
	} else {
		v.method, v.hmacKey = jwt.SigningMethodHS256, []byte(cfg.SessionJWTSecret)
	}
	if cfg.MacroAPITokenPublicKey != "" {
		pub, err := jwt.ParseRSAPublicKeyFromPEM([]byte(cfg.MacroAPITokenPublicKey))
		if err != nil {
			return nil, fmt.Errorf("identity: parse MACRO_API_TOKEN_PUBLIC_KEY: %w", err)
		}
		v.apiTokenPub, v.apiTokenIss = pub, cfg.MacroAPITokenIssuer
	}
	return v, nil
}

func (v *Validator) keyFunc(token *jwt.Token) (any, error) {
	if token.Method.Alg() != v.method.Alg() {
		return nil, fmt.Errorf("identity: unexpected signing method %s", token.Method.Alg())
	}
	if v.method == jwt.SigningMethodRS256 {
		return v.rsaPub, nil
	}
	return v.hmacKey, nil
}

// ErrTokenExpired distinguishes an expired-but-otherwise-valid token (the
// refresh endpoint relies on it), matching MacroAuthError::JwtExpired.
var ErrTokenExpired = errors.New("identity: token expired")

// ValidateAccessToken verifies a service-issued access token and returns its
// claims. Expired tokens return ErrTokenExpired (wrapped).
func (v *Validator) ValidateAccessToken(raw string) (*AccessClaims, error) {
	claims := &AccessClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, v.keyFunc,
		jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience), jwt.WithExpirationRequired())
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, fmt.Errorf("%w: %v", ErrTokenExpired, err)
		}
		return nil, fmt.Errorf("identity: invalid access token: %w", err)
	}
	return claims, nil
}

// DecodeAccessTokenAllowExpired verifies signature/aud/iss but ignores
// expiry — mirrors decode_macro_access_token_allow_expired (used to extract
// the user id from an expired token during refresh).
func (v *Validator) DecodeAccessTokenAllowExpired(raw string) (*AccessClaims, error) {
	claims := &AccessClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, v.keyFunc,
		jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience),
		jwt.WithoutClaimsValidation())
	if err != nil {
		return nil, fmt.Errorf("identity: invalid access token: %w", err)
	}
	return claims, nil
}

// ValidateRefreshToken verifies a service-issued refresh token and returns
// the claims needed to rotate/revoke it.
func (v *Validator) ValidateRefreshToken(raw string) (*RefreshClaims, error) {
	return v.parseRefreshToken(raw, false)
}

// DecodeRefreshTokenAllowExpired verifies signature + issuer but ignores
// expiry — used by logout to revoke a token that just lapsed.
func (v *Validator) DecodeRefreshTokenAllowExpired(raw string) (*RefreshClaims, error) {
	return v.parseRefreshToken(raw, true)
}

func (v *Validator) parseRefreshToken(raw string, allowExpired bool) (*RefreshClaims, error) {
	claims := &refreshClaims{}
	opts := []jwt.ParserOption{jwt.WithIssuer(v.issuer)}
	if allowExpired {
		opts = append(opts, jwt.WithoutClaimsValidation())
	} else {
		opts = append(opts, jwt.WithExpirationRequired())
	}
	_, err := jwt.ParseWithClaims(raw, claims, v.keyFunc, opts...)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, fmt.Errorf("%w: %v", ErrTokenExpired, err)
		}
		return nil, fmt.Errorf("identity: invalid refresh token: %w", err)
	}
	if claims.Type != "refresh" {
		return nil, fmt.Errorf("identity: not a refresh token")
	}
	out := &RefreshClaims{Subject: claims.Subject, ID: claims.ID}
	if claims.ExpiresAt != nil {
		out.ExpiresAt = claims.ExpiresAt.Time
	}
	return out, nil
}

// ResolvedIdentity is the Go port of ValidatedIdentity — what a validated
// token tells a handler about the caller.
type ResolvedIdentity struct {
	// UserID is the macro user id "macro|<email>" (User.id).
	UserID string
	// ProviderUserID mirrors UserContext.fusion_user_id: root_macro_id when
	// present, else the provider subject (macro_user table UUID or Casdoor id).
	ProviderUserID string
	Email          string
	OrganizationID *int64
}

// ValidateToken is decode_jwt::handler: it dispatches on the token's kid —
// "macro" means a macro-api-token (RS256), anything else is a session access
// token.
func (v *Validator) ValidateToken(raw string) (*ResolvedIdentity, error) {
	var header struct {
		Kid string `json:"kid"`
	}
	// Decode just the header.
	seg := raw
	if i := indexByte(seg, '.'); i >= 0 {
		seg = seg[:i]
	}
	if err := jsonUnmarshalB64(seg, &header); err == nil && header.Kid == "macro" && v.apiTokenPub != nil {
		claims := &MacroAPIClaims{}
		_, err := jwt.ParseWithClaims(raw, claims,
			func(t *jwt.Token) (any, error) {
				if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
					return nil, fmt.Errorf("identity: unexpected alg %s", t.Method.Alg())
				}
				return v.apiTokenPub, nil
			},
			jwt.WithIssuer(v.apiTokenIss), jwt.WithExpirationRequired())
		if err != nil {
			if errors.Is(err, jwt.ErrTokenExpired) {
				return nil, fmt.Errorf("%w: %v", ErrTokenExpired, err)
			}
			return nil, fmt.Errorf("identity: invalid macro-api-token: %w", err)
		}
		return &ResolvedIdentity{
			UserID:         claims.MacroUserID,
			ProviderUserID: claims.FusionUserID,
			OrganizationID: claims.MacroOrganizationID,
		}, nil
	}

	claims, err := v.ValidateAccessToken(raw)
	if err != nil {
		return nil, err
	}
	providerID := claims.FusionUserID
	if claims.RootMacroID != nil && *claims.RootMacroID != "" {
		providerID = *claims.RootMacroID
	}
	return &ResolvedIdentity{
		UserID:         claims.MacroUserID,
		ProviderUserID: providerID,
		Email:          claims.Email,
		OrganizationID: claims.MacroOrganizationID,
	}, nil
}

// EncodeMacroAPIToken signs a macro-api-token (kid="macro") —
// macro_auth::encode_macro_api_token.
func EncodeMacroAPIToken(privateKeyPEM, issuer, fusionUserID, macroUserID string, orgID *int64, ttl time.Duration) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return "", fmt.Errorf("identity: parse MACRO_API_TOKEN_PRIVATE_KEY: %w", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, MacroAPIClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		},
		FusionUserID:        fusionUserID,
		MacroUserID:         macroUserID,
		MacroOrganizationID: orgID,
	})
	tok.Header["kid"] = "macro"
	return tok.SignedString(key)
}

// RefreshStore tracks live refresh-token ids so tokens can be rotated
// (single-use) and revoked (logout). The Rust service got this from
// FusionAuth, which owned the refresh token; our service-issued tokens need
// equivalent server-side state. Implementations must be atomic on Consume —
// a GETDEL-style read-and-delete — so two concurrent refreshes of the same
// token can't both succeed.
type RefreshStore interface {
	// Register marks a freshly-issued token id as live for subject. A
	// failure should fail session issuance — an unregistered token can
	// never be consumed.
	Register(ctx context.Context, id, subject string, ttl time.Duration) error
	// Consume atomically claims a token id for rotation: it returns true
	// exactly once per registered id, for the subject it was registered
	// under. A second call (a replayed token) returns false.
	Consume(ctx context.Context, id, subject string) (bool, error)
	// Revoke deletes a token id without consuming it (logout).
	Revoke(ctx context.Context, id string) error
}

// ErrRefreshTokenReused is returned by refresh handlers when a token id was
// already consumed (replay) or never registered.
var ErrRefreshTokenReused = errors.New("identity: refresh token already used or unknown")

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// jsonUnmarshalB64 decodes a base64url JWT segment and unmarshals it.
func jsonUnmarshalB64(seg string, out any) error {
	raw, err := jwt.NewParser().DecodeSegment(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
