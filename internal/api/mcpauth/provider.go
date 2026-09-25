package mcpauth

import (
	"context"
	"errors"

	"github.com/macro-inc/macro/pkg/identity"
)

// OAuthProvider mirrors domain::ports::OAuthProvider — the upstream identity
// provider the broker relays the OAuth flow to (Casdoor replaces FusionAuth).
type OAuthProvider interface {
	// ConstructAuthorizeURL builds the upstream authorize URL carrying the
	// broker session id as `state`.
	ConstructAuthorizeURL(ctx context.Context, state string) (string, error)
	// ExchangeAuthorizationCode redeems an upstream authorization code.
	ExchangeAuthorizationCode(ctx context.Context, code string) (*UpstreamTokens, error)
	// RefreshAccessToken performs an upstream refresh-token grant.
	RefreshAccessToken(ctx context.Context, refreshToken string) (*UpstreamTokens, error)
}

// IdentityOAuthProvider adapts identity.Port (Casdoor) onto OAuthProvider,
// replacing outbound::fusionauth::FusionAuthOAuthProvider.
type IdentityOAuthProvider struct {
	IDP identity.Port
	// UpstreamProvider is the Casdoor provider name deep-linked on the
	// authorize URL (was the FusionAuth google_gmail IdP id). Empty leaves
	// the provider picker to Casdoor.
	UpstreamProvider string
}

// ConstructAuthorizeURL implements OAuthProvider.
func (p IdentityOAuthProvider) ConstructAuthorizeURL(_ context.Context, state string) (string, error) {
	return p.IDP.AuthorizeURL(identity.AuthorizeRequest{
		Provider: p.UpstreamProvider,
		State:    state,
	})
}

// ExchangeAuthorizationCode implements OAuthProvider.
func (p IdentityOAuthProvider) ExchangeAuthorizationCode(ctx context.Context, code string) (*UpstreamTokens, error) {
	grant, err := p.IDP.ExchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}
	return &UpstreamTokens{
		AccessToken:  grant.AccessToken,
		RefreshToken: grant.RefreshToken,
		ExpiresIn:    uint64(max(grant.ExpiresIn, 0)),
	}, nil
}

// RefreshAccessToken implements OAuthProvider.
func (p IdentityOAuthProvider) RefreshAccessToken(ctx context.Context, refreshToken string) (*UpstreamTokens, error) {
	grant, err := p.IDP.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	return &UpstreamTokens{
		AccessToken:  grant.AccessToken,
		RefreshToken: grant.RefreshToken,
		ExpiresIn:    uint64(max(grant.ExpiresIn, 0)),
	}, nil
}

// ErrProviderNotConfigured is returned when no upstream OAuth provider is
// configured for the broker.
var ErrProviderNotConfigured = errors.New("mcpauth: no upstream OAuth provider configured")

// NullOAuthProvider fails closed when the identity stack is unconfigured.
type NullOAuthProvider struct{}

func (NullOAuthProvider) ConstructAuthorizeURL(context.Context, string) (string, error) {
	return "", ErrProviderNotConfigured
}
func (NullOAuthProvider) ExchangeAuthorizationCode(context.Context, string) (*UpstreamTokens, error) {
	return nil, ErrProviderNotConfigured
}
func (NullOAuthProvider) RefreshAccessToken(context.Context, string) (*UpstreamTokens, error) {
	return nil, ErrProviderNotConfigured
}
