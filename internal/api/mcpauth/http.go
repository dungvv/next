package mcpauth

import (
	"errors"
	"net/http"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/config"
)

// Config carries the mcp-auth-proxy settings; it embeds the root config so
// package-level env additions live here, not in pkg/config.
type Config struct {
	config.Config
	// PublicURL is MCP_PUBLIC_URL — the external base the OAuth endpoints are
	// reachable at (used in discovery metadata).
	PublicURL string `env:"MCP_PUBLIC_URL" envDefault:""`
	// MetadataPath is the public path of the protected-resource metadata
	// document, advertised in WWW-Authenticate challenges
	// (default "/mcp/.well-known/oauth-protected-resource").
	MetadataPath string `env:"MCP_METADATA_PATH" envDefault:""`
	// UpstreamProvider deep-links a Casdoor provider on the authorize URL
	// (was the FusionAuth google_gmail IdP id). Empty → Casdoor picker.
	UpstreamProvider string `env:"MCP_UPSTREAM_PROVIDER" envDefault:""`
	// AllowedRedirectURIs is the MCP_ALLOWED_REDIRECT_URIS allowlist
	// (comma-separated). Non-loopback https redirect_uris must exact-match
	// an entry; empty falls back to the Rust "any https" rule.
	AllowedRedirectURIs []string `env:"MCP_ALLOWED_REDIRECT_URIS" envSeparator:","`
}

// Load parses environment variables into the mcpauth Config.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Deps wires the router.
type Deps struct {
	Service *Service
	Config  Config
}

// Register mounts the OAuth broker surface (the Rust mcp_router's
// oauth_routes, already under the caller's `/mcp` mount prefix). The
// protected `/mcp` streamable endpoint itself is mounted by
// internal/api/mcp using BearerMiddleware.
func (d Deps) Register(r chi.Router) {
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	d.RegisterOAuth(r)
}

// RegisterOAuth mounts only the OAuth broker routes — used for the root
// half of the Rust dual mount where /health is already owned.
func (d Deps) RegisterOAuth(r chi.Router) {
	r.Get("/.well-known/oauth-protected-resource", d.protectedResourceMetadata)
	r.Get("/.well-known/oauth-protected-resource/mcp", d.protectedResourceMetadata)
	r.Get("/.well-known/oauth-authorization-server", d.authorizationServerMetadata)
	r.Get("/.well-known/oauth-authorization-server/mcp", d.authorizationServerMetadata)
	r.Get("/authorize", d.authorize)
	r.Post("/register", d.register)
	r.Get("/oauth/callback", d.oauthCallback)
	r.Post("/token", d.token)
}

func (d Deps) protectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, d.Service.ProtectedResourceMetadata())
}

func (d Deps) authorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, d.Service.AuthorizationServerMetadata())
}

func (d Deps) register(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d.Service.RegisterClient(body))
}

func (d Deps) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	url, err := d.Service.StartAuthorization(r.Context(), AuthorizeRequest{
		ResponseType:        q.Get("response_type"),
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		State:               q.Get("state"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		Scope:               q.Get("scope"),
	})
	if err != nil {
		var se *StartAuthorizationError
		if errors.As(err, &se) {
			switch se {
			case errStartResponseType:
				httpx.Error(w, http.StatusBadRequest, "unsupported response_type")
			case errStartChallengeMethod:
				httpx.Error(w, http.StatusBadRequest, "unsupported code_challenge_method")
			case errStartRedirectURI:
				httpx.Error(w, http.StatusBadRequest, "redirect_uri must be https or a loopback address")
			case errStartInflight:
				httpx.Error(w, http.StatusInternalServerError, "failed to persist inflight auth state")
			default:
				httpx.Error(w, http.StatusInternalServerError, "failed to construct authorize URL")
			}
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "failed to construct authorize URL")
		return
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func opt(q map[string][]string, key string) *string {
	if v, ok := q[key]; ok && len(v) > 0 {
		s := v[0]
		return &s
	}
	return nil
}

func (d Deps) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	url, err := d.Service.CompleteCallback(r.Context(), CallbackRequest{
		Code:             opt(q, "code"),
		State:            opt(q, "state"),
		Error:            opt(q, "error"),
		ErrorDescription: opt(q, "error_description"),
	})
	if err != nil {
		var ce *CompleteCallbackError
		if errors.As(err, &ce) {
			switch ce {
			case errCallbackMissingState:
				httpx.Error(w, http.StatusBadRequest, "missing state parameter")
			case errCallbackMissingCode:
				httpx.Error(w, http.StatusBadRequest, "missing code parameter")
			case errCallbackUnknownSession:
				httpx.Error(w, http.StatusBadRequest, "unknown or expired session")
			case errCallbackInflight:
				httpx.Error(w, http.StatusInternalServerError, "failed to access inflight auth state")
			default:
				httpx.Error(w, http.StatusBadGateway, "authorization code exchange failed")
			}
			return
		}
		httpx.Error(w, http.StatusBadGateway, "authorization code exchange failed")
		return
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (d Deps) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid form body")
		return
	}
	form := r.PostForm
	resp, err := d.Service.ExchangeToken(r.Context(), TokenRequest{
		GrantType:    form.Get("grant_type"),
		Code:         opt(form, "code"),
		CodeVerifier: opt(form, "code_verifier"),
		RefreshToken: opt(form, "refresh_token"),
		RedirectURI:  opt(form, "redirect_uri"),
		ClientID:     opt(form, "client_id"),
	})
	if err != nil {
		var te *TokenExchangeError
		if errors.As(err, &te) {
			switch te {
			case errTokenUnsupportedGrant:
				httpx.Error(w, http.StatusBadRequest, "unsupported grant_type")
			case errTokenCodeRequired:
				httpx.Error(w, http.StatusBadRequest, "code required")
			case errTokenInvalidCode:
				httpx.Error(w, http.StatusBadRequest, "invalid or expired code")
			case errTokenRedirectMismatch:
				httpx.Error(w, http.StatusBadRequest, "redirect_uri mismatch")
			case errTokenRedirectRequired:
				httpx.Error(w, http.StatusBadRequest, "redirect_uri required")
			case errTokenClientRequired:
				httpx.Error(w, http.StatusBadRequest, "client_id required")
			case errTokenClientMismatch:
				httpx.Error(w, http.StatusBadRequest, "client_id mismatch")
			case errTokenVerifierRequired:
				httpx.Error(w, http.StatusBadRequest, "code_verifier required")
			case errTokenPKCE:
				httpx.Error(w, http.StatusBadRequest, "PKCE verification failed")
			case errTokenRefreshRequired:
				httpx.Error(w, http.StatusBadRequest, "refresh_token required")
			case errTokenInflight:
				httpx.Error(w, http.StatusInternalServerError, "failed to access inflight auth state")
			default:
				httpx.Error(w, http.StatusBadGateway, "refresh token exchange failed")
			}
			return
		}
		httpx.Error(w, http.StatusBadGateway, "refresh token exchange failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
