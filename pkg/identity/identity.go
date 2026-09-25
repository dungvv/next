// Package identity is the outbound port to the identity provider (Casdoor,
// replacing FusionAuth) plus the service-session token machinery that turns a
// provider login into Macro JWTs.
//
// The Port interface is deliberately provider-generic: authorize URL
// construction, code exchange, token refresh, userinfo and logout. A second
// implementation (e.g. another OIDC provider, or casdoor-go-sdk) can be
// swapped in without touching internal/api/authentication.
package identity

import (
	"context"
	"errors"
)

// AuthorizeRequest parameterizes building the OAuth authorize URL.
type AuthorizeRequest struct {
	// Provider is the upstream IdP hint. On Casdoor this is the provider
	// name appended to the authorize URL so the user skips the provider
	// picker; it replaces FusionAuth's `idp_hint`.
	Provider string
	// LoginHint pre-fills the user's email at the provider.
	LoginHint string
	// State is an opaque blob round-tripped through the provider. The caller
	// JSON-encodes its own state struct.
	State string
	// RedirectURI overrides the configured redirect URI when non-empty.
	RedirectURI string
	// Scopes overrides the default "openid profile email offline_access".
	Scopes []string
}

// TokenGrant mirrors fusionauth::oauth::OAuth2Grant.
type TokenGrant struct {
	AccessToken  string
	RefreshToken string
	// IDToken is the OIDC id_token, present on code exchange and refresh
	// when the provider returns one.
	IDToken   string
	ExpiresIn int64
	TokenType string
}

// Profile is the normalized user record resolved from the provider.
type Profile struct {
	// ProviderUserID is the stable subject (Casdoor user UUID; was the
	// FusionAuth user id).
	ProviderUserID string
	Email          string
	Name           string // display name
	FirstName      string
	LastName       string
	Avatar         string
	// EmailVerified reports whether the provider considers the email
	// verified. Casdoor marks SSO-verified emails itself.
	EmailVerified bool
	// Providers lists the IdP names the account is linked to (used by
	// /user/link_exists).
	Providers []string
}

// Port is the identity-provider port. Implementations must be safe for
// concurrent use.
type Port interface {
	// AuthorizeURL builds the browser redirect that starts the OAuth flow.
	AuthorizeURL(req AuthorizeRequest) (string, error)
	// ExchangeCode completes the authorization-code grant.
	ExchangeCode(ctx context.Context, code string) (*TokenGrant, error)
	// PasswordLogin performs a resource-owner-password-credentials grant.
	// Providers that disable ROPC return ErrUnsupported.
	PasswordLogin(ctx context.Context, username, password string) (*TokenGrant, error)
	// RefreshTokens completes the refresh-token grant.
	RefreshTokens(ctx context.Context, refreshToken string) (*TokenGrant, error)
	// VerifyIDToken verifies a provider-issued OIDC id token and returns the
	// user profile it asserts.
	VerifyIDToken(ctx context.Context, idToken string) (*Profile, error)
	// UserInfo calls the provider's userinfo endpoint with an access token.
	UserInfo(ctx context.Context, accessToken string) (*Profile, error)
	// LogoutURL returns the provider logout URL the browser can be sent to,
	// or "" when the provider has no logout endpoint.
	LogoutURL(idTokenHint, postLogoutRedirectURI string) string
}

// ErrUnsupported is returned by Port methods the provider cannot do
// (e.g. password grant disabled on Casdoor).
var ErrUnsupported = errors.New("identity: operation unsupported by provider")

// ErrInvalidCredentials maps to 401 on the HTTP edge (bad password, bad code).
var ErrInvalidCredentials = errors.New("identity: invalid credentials")
