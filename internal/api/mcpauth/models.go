// Package mcpauth ports services/mcp_auth_proxy: an OAuth broker that sits in
// front of the MCP streamable HTTP endpoint. Clients run a dynamic
// registration + PKCE authorization flow against this broker; the broker
// relays the flow to the identity provider (Casdoor, replacing FusionAuth)
// and hands the upstream tokens back to the client. The same package also
// carries the Bearer middleware that guards /mcp.
package mcpauth

import "time"

// UpstreamTokens is a token grant obtained from the upstream OAuth provider.
type UpstreamTokens struct {
	AccessToken  string
	RefreshToken string
	// ExpiresIn is seconds until AccessToken expires, as reported upstream.
	ExpiresIn uint64
}

// PendingAuthorization is an OAuth authorization flow initiated by the
// client, keyed by a broker session id.
type PendingAuthorization struct {
	// CodeChallenge is the client's PKCE S256 code challenge.
	CodeChallenge string `json:"code_challenge"`
	// ClientState is the client's original `state` parameter.
	ClientState string `json:"client_state"`
	// ClientRedirectURI is where to redirect back to the client with the
	// authorization code.
	ClientRedirectURI string `json:"client_redirect_uri"`
	// ClientID binds the flow to the client that started it (checked at
	// token exchange; RFC 6749 §4.1.3).
	ClientID string `json:"client_id,omitempty"`
}

// IssuedAuthorizationCode is a broker-issued authorization code backed by an
// upstream token grant.
type IssuedAuthorizationCode struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// CodeChallenge is the original PKCE code challenge, verified at token
	// exchange.
	CodeChallenge string `json:"code_challenge"`
	// RedirectURI from the authorization request, exact-match validated at
	// token exchange.
	RedirectURI string `json:"redirect_uri"`
	// ClientID from the authorization request; the token request must
	// present the same client_id.
	ClientID string `json:"client_id,omitempty"`
	// AccessTokenExpiresAt records when the upstream access token expires;
	// zero for codes issued before the broker tracked upstream lifetimes.
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at,omitempty"`
}

// AuthorizeRequest mirrors domain::models::AuthorizeRequest (query params of
// GET /authorize).
type AuthorizeRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Scope               string
}

// CallbackRequest mirrors domain::models::CallbackRequest (query params of
// GET /oauth/callback).
type CallbackRequest struct {
	Code             *string
	State            *string
	Error            *string
	ErrorDescription *string
}

// TokenRequest mirrors domain::models::TokenRequest (form body of POST /token).
type TokenRequest struct {
	GrantType    string
	Code         *string
	CodeVerifier *string
	RefreshToken *string
	RedirectURI  *string
	ClientID     *string
}

// TokenResponse mirrors domain::models::TokenResponse.
type TokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	TokenType    string  `json:"token_type"`
	ExpiresIn    *uint64 `json:"expires_in,omitempty"`
}
