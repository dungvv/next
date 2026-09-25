package identity

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config configures the identity provider (Casdoor by default) plus the
// service-issued session JWTs that replace the FusionAuth-issued
// macro-access-token / macro-refresh-token pair.
//
// All fields map onto environment variables so they can live in the same
// env-file as the rest of the self-host stack.
type Config struct {
	// --- Identity provider (Casdoor) ---
	// Endpoint is the server-to-server Casdoor base URL
	// (e.g. http://casdoor:8000 inside compose).
	Endpoint string `env:"CASDOOR_ENDPOINT" envDefault:"http://localhost:8000"`
	// PublicEndpoint is the browser-reachable Casdoor URL used for
	// authorize/logout redirects. Falls back to Endpoint when empty.
	PublicEndpoint string `env:"CASDOOR_PUBLIC_ENDPOINT" envDefault:""`
	ClientID       string `env:"CASDOOR_CLIENT_ID" envDefault:""`
	ClientSecret   string `env:"CASDOOR_CLIENT_SECRET" envDefault:""`
	// Organization is the Casdoor organization that owns the app.
	Organization string `env:"CASDOOR_ORG" envDefault:"macro"`
	// Application is the Casdoor application name (used in admin API calls
	// and to build default redirect URIs).
	Application string `env:"CASDOOR_APP" envDefault:"app-built-in"`
	// RedirectURI is the OAuth redirect URI registered on the Casdoor
	// application — i.e. this service's public `/oauth/redirect` URL.
	RedirectURI string `env:"CASDOOR_REDIRECT_URI" envDefault:""`

	// --- Service-issued session tokens ---
	// SessionJWTSecret is the HMAC key for HS256 session tokens.
	// Replaces JWT_SECRET_KEY in macro_auth.
	SessionJWTSecret string `env:"JWT_SECRET_KEY" envDefault:""`
	// SessionJWTPrivateKey is an optional RSA private key (PEM). When set,
	// session tokens are signed RS256 instead of HS256.
	SessionJWTPrivateKey string `env:"JWT_PRIVATE_KEY" envDefault:""`
	// SessionJWTIssuer is the `iss` claim for issued tokens
	// (macro_auth Issuer env var).
	SessionJWTIssuer string `env:"JWT_ISSUER" envDefault:"macro"`
	// SessionJWTAudience is the `aud` claim for issued tokens; defaults to
	// the Casdoor client id (matching FusionAuth, where aud was the
	// application id).
	SessionJWTAudience string `env:"JWT_AUDIENCE" envDefault:""`
	// AccessTokenTTL is how long issued access tokens stay valid.
	AccessTokenTTL time.Duration `env:"JWT_ACCESS_TOKEN_TTL" envDefault:"1h"`
	// RefreshTokenTTL is how long issued refresh tokens stay valid.
	RefreshTokenTTL time.Duration `env:"JWT_REFRESH_TOKEN_TTL" envDefault:"720h"`

	// --- macro-api-token (RS256, kid="macro") ---
	MacroAPITokenIssuer     string        `env:"MACRO_API_TOKEN_ISSUER" envDefault:"macro"`
	MacroAPITokenPrivateKey string        `env:"MACRO_API_TOKEN_PRIVATE_KEY" envDefault:""`
	MacroAPITokenPublicKey  string        `env:"MACRO_API_TOKEN_PUBLIC_KEY" envDefault:""`
	MacroAPITokenTTL        time.Duration `env:"MACRO_API_TOKEN_TTL" envDefault:"1h"`
}

// Load parses identity configuration from the environment.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse identity env config: %w", err)
	}
	if c.PublicEndpoint == "" {
		c.PublicEndpoint = c.Endpoint
	}
	if c.SessionJWTAudience == "" {
		c.SessionJWTAudience = c.ClientID
	}
	return c, nil
}
