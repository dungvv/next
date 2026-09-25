// Package authentication ports services/authentication_service to Go.
//
// The Rust service sat behind FusionAuth; this port talks to Casdoor through
// pkg/identity.Port and issues its own HS256/RS256 session JWTs (same
// MacroAccessToken claim shape) after the OAuth code exchange.
//
// Route surface (mounted at both / and /auth by the caller, matching the
// Rust dual mount for the gateway ALB):
//
//	/login/sso, /login/password, /login/passwordless, /login/apple
//	/logout (GET+POST)
//	/oauth/redirect, /oauth/passwordless/{code}
//	/oauth2/{provider}/callback
//	/jwt/refresh, /jwt/macro_api_token
//	/session (POST), /session/login/{session_code}
//	/permissions/, /permissions/me
//	/user/* (me, name, get_names, quota, tutorial, ai_consent, group,
//	        onboarding, organization, profile pictures, link_exists,
//	        legacy_user_permissions, stripe stubs)
//	/internal/* (get_names, get_existing_users, link/email-grant stubs)
//	/link/*, /cursor-api-key/*, /codex/*, /github_pull_requests/enrich
//	/team/*, /referral/*, /gtm-invite/*
//	/mobile-welcome-email
//	/webhooks/user{,/delete,/jwt,/name,/stripe}
//
// Routes that depend on domain crates not yet ported (teams/referral/gtm
// services, codex_connection, github link service, cursor key store) are
// registered and return 501 with a TODO — keeping the surface complete so
// callers get a deterministic answer instead of a 404.
package authentication

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/macro-inc/macro/internal/api/authentication/middleware"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/identity"
	"github.com/macro-inc/macro/pkg/mail"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// Config is the authentication_service env surface minus the parts that live
// in pkg/config (db urls, valkey) or pkg/identity (CASDOOR_*, JWT_*).
type Config struct {
	// Env is inherited from the root config (ENVIRONMENT) — controls cookie
	// naming and default redirect URLs. Not parsed here.
	Env string `env:"-"`

	// BaseURL is this service's public base (BASE_URL in Rust) — used to
	// build oauth2 provider callbacks.
	BaseURL string `env:"BASE_URL" envDefault:"http://localhost:8080"`
	// FrontendURL overrides the default post-login redirect (the app root).
	FrontendURL string `env:"FRONTEND_URL" envDefault:""`
	// FrontendPort is the local dev app port when FrontendURL is unset
	// (FRONTEND_PORT in Rust utils).
	FrontendPort int `env:"FRONTEND_PORT" envDefault:"3000"`

	// CookieDomain overrides the environment-derived cookie domain
	// (macro.com for dev/prod, host-only for local).
	CookieDomain string `env:"AUTH_COOKIE_DOMAIN" envDefault:""`

	// AllowedOriginalURLHosts appends hosts to the OAuth original_url
	// allowlist (comma-separated) — needed for self-host domains.
	AllowedOriginalURLHosts string `env:"AUTH_ALLOWED_REDIRECT_HOSTS" envDefault:""`

	// InternalAPIKey is x-internal-auth-key for service-to-service calls.
	InternalAPIKey string `env:"INTERNAL_API_KEY" envDefault:""`

	// SignupAllowlist is a JSON array of emails allowed to sign up when the
	// environment gates signups (DEVELOPMENT_SIGNUP_ALLOWLIST_JSON).
	SignupAllowlist string `env:"DEVELOPMENT_SIGNUP_ALLOWLIST_JSON" envDefault:""`

	// MicrosoftTokenMasterKey is a base64-encoded 32-byte key wrapping the
	// per-token data keys — replaces AWS KMS for microsoft_token_cipher.
	MicrosoftTokenMasterKey string `env:"MICROSOFT_TOKEN_MASTER_KEY" envDefault:""`
}

// Load parses the authentication config from the environment.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse authentication env config: %w", err)
	}
	return c, nil
}

// Deps wires the router. Nil optional deps degrade gracefully:
//   - Identity nil → login/oauth handlers 503
//   - Redis nil → session-code endpoints 503
//   - Mail nil → mobile-welcome-email 503
type Deps struct {
	Cfg       Config
	Pool      *pgxpool.Pool
	Q         *macrodb.Queries
	Identity  identity.Port
	Sessions  *identity.Issuer
	Validator *identity.Validator
	Redis     *redis.Client
	Mail      mail.Port

	// RefreshTokens tracks issued refresh-token ids so /jwt/refresh can
	// rotate them (single-use) and /logout can revoke them — the FusionAuth
	// session semantics the Rust service relied on. Nil (or a nil Redis, in
	// which case New wires the Valkey adapter itself) degrades to the old
	// stateless behavior: tokens stay valid until expiry.
	RefreshTokens identity.RefreshStore

	// CasdoorAdmin exposes the provider admin ops (user create/delete,
	// provider listing). Nil when the port can't do them.
	Admin AdminPort

	// APIToken* configure macro-api-token signing (MACRO_API_TOKEN_*).
	APITokenKey    string
	APITokenIssuer string
	APITokenTTL    time.Duration

	cookies *middleware.Cookies
}

// AdminPort is the provider-admin surface (Casdoor /api/* with client
// credentials). Separate from identity.Port so other providers can implement
// the user-facing half only.
type AdminPort interface {
	AdminGetUserByEmail(ctx context.Context, email string) (*identity.Profile, error)
	AdminCreateUser(ctx context.Context, p *identity.Profile, password string) error
	AdminDeleteUser(ctx context.Context, name string) error
	// AdminSendVerificationCode emails the provider's verification code to
	// the address (the FusionAuth create_user skip_verification=false
	// equivalent — newly registered users can't log in until verified).
	AdminSendVerificationCode(ctx context.Context, email string) error
}

// New validates Deps and returns a ready router Registrar.
func New(d Deps) (*Router, error) {
	if d.Pool == nil || d.Q == nil {
		return nil, fmt.Errorf("authentication: macrodb pool/queries required")
	}
	if d.Sessions == nil || d.Validator == nil {
		return nil, fmt.Errorf("authentication: session issuer/validator required")
	}
	d.cookies = middleware.NewCookies(d.Cfg.Env, d.Cfg.CookieDomain)
	if d.RefreshTokens == nil && d.Redis != nil {
		d.RefreshTokens = &redisRefreshStore{rdb: d.Redis}
	}
	if d.RefreshTokens == nil {
		slog.Warn("authentication: no refresh-token store configured; " +
			"refresh tokens are stateless and cannot be rotated or revoked " +
			"(set up Valkey to enable)")
	}
	return &Router{deps: d}, nil
}

// Router carries the deps into every handler.
type Router struct {
	deps Deps
}

// Register mounts the whole authentication surface on r, including
// /health and /internal. The caller mounts it at "/" and "/auth" like the
// Rust dual mount.
func (rt *Router) Register(r chi.Router) {
	rt.register(r, registerOpts{health: true, internal: true})
}

// RegisterNoHealth mounts the full route surface without /health — for a
// standalone root mount where the host owns /health.
func (rt *Router) RegisterNoHealth(r chi.Router) {
	rt.register(r, registerOpts{internal: true})
}

// RegisterNoInternal mounts the route surface without /internal or /health
// — for the root half of the dual mount in the combined api binary, which
// already owns both paths. Pair it with RegisterInternal inside the host's
// existing /internal mount.
func (rt *Router) RegisterNoInternal(r chi.Router) {
	rt.register(r, registerOpts{})
}

// RegisterInternal mounts only the service-to-service /internal routes onto
// r — for merging into a mount the host router already owns (chi panics on
// duplicate Mount() at the same pattern).
func (rt *Router) RegisterInternal(r chi.Router) {
	rt.internalRoutes(r)
}

type registerOpts struct {
	health   bool
	internal bool
}

func (rt *Router) register(r chi.Router, opts registerOpts) {
	requireUser := middleware.RequireUser(rt.deps.Validator, rt.deps.cookies, rt.deps.Cfg.InternalAPIKey)
	// axum DefaultBodyLimit parity (2 MiB) on every body-parsing endpoint.
	maxBody := httpx.MaxBody(httpx.DefaultBodyLimitBytes)

	// --- login -------------------------------------------------------------
	r.Route("/login", func(r chi.Router) {
		r.Use(maxBody)
		r.Get("/sso", rt.loginSSO)
		r.Post("/password", rt.loginPassword)
		// TODO(port): passwordless start — needs Casdoor verification-code
		// login (send email code via /api/send-verification-code, then
		// grant). FusionAuth-specific flow in Rust.
		r.Post("/passwordless", notImplemented("passwordless login"))
		// TODO(port): apple login — verify Apple id_token against Apple JWKS
		// then JIT-provision; provider-specific.
		r.Post("/apple", notImplemented("apple login"))
	})

	// --- logout -------------------------------------------------------------
	r.Route("/logout", func(r chi.Router) {
		r.With(requireUser).Post("/", rt.logout)
		r.With(requireUser).Get("/", rt.logout)
	})

	// --- oauth (provider callback) ------------------------------------------
	r.Route("/oauth", func(r chi.Router) {
		r.Get("/redirect", rt.oauthRedirect)
		// TODO(port): passwordless email-code callback. Depends on the
		// passwordless start endpoint above; Valkey key pw_login_code:<email>.
		r.Get("/passwordless/{code}", notImplemented("passwordless callback"))
	})

	// --- oauth2 (external account-link callbacks) ----------------------------
	// TODO(port): the google/microsoft/github account-link callbacks exchange
	// provider codes directly (not via Casdoor) and persist link grants in
	// emaildb — needs the link/* in-progress flow and the token ciphers.
	r.Route("/oauth2", func(r chi.Router) {
		r.Get("/{provider}/callback", notImplemented("oauth2 account-link callback"))
	})

	// --- jwt -----------------------------------------------------------------
	r.Route("/jwt", func(r chi.Router) {
		r.Post("/refresh", rt.jwtRefresh)
		r.With(requireUser).Get("/macro_api_token", rt.macroAPIToken)
	})

	// --- session --------------------------------------------------------------
	r.Route("/session", func(r chi.Router) {
		r.Use(maxBody)
		r.Post("/", rt.sessionCreate)
		r.Get("/login/{session_code}", rt.sessionLogin)
	})

	// --- permissions -----------------------------------------------------------
	r.Route("/permissions", func(r chi.Router) {
		r.Get("/", rt.getPermissions)
		r.With(requireUser).Get("/me", rt.getUserPermissions)
	})

	// --- user ------------------------------------------------------------------
	r.Route("/user", func(r chi.Router) {
		r.Use(maxBody)
		r.Post("/", rt.createUser)
		r.Group(func(r chi.Router) {
			r.Use(requireUser)
			r.Get("/me", rt.getUserInfo)
			r.Delete("/me", rt.deleteUser)
			r.Post("/profile_pictures", rt.getProfilePictures)
			r.Put("/profile_picture", rt.putProfilePicture)
			r.Put("/name", rt.putUserName)
			r.Get("/name", rt.getUserName)
			r.Post("/get_names", rt.getNames)
			r.Post("/get_names_with_email", rt.getNamesWithEmail)
			r.Get("/link_exists", rt.getUserLinkExists)
			r.Patch("/tutorial", rt.patchTutorial)
			r.Patch("/ai_consent", rt.patchAIConsent)
			r.Get("/quota", rt.getUserQuota)
			r.Get("/legacy_user_permissions", rt.getLegacyUserPermissions)
			r.Get("/organization", rt.getUserOrganization)
			r.Patch("/group", rt.patchUserGroup)
			r.Patch("/onboarding", rt.patchUserOnboarding)
			// TODO(port): Stripe checkout/portal — external SaaS billing,
			// keep behind a feature flag when ported.
			r.Post("/stripe/checkoutv2", notImplemented("stripe checkout"))
			r.Post("/stripe/portal", notImplemented("stripe portal"))
		})
	})

	// --- internal -------------------------------------------------------------
	if opts.internal {
		r.Route("/internal", rt.internalRoutes)
	}

	// --- link ------------------------------------------------------------------
	// TODO(port): account-link initiation (gmail/outlook/github) — starts an
	// OAuth dance at the external provider, so it needs provider clients and
	// the in_progress_user_link flow (macrodb queries exist; see
	// in_progress_user_link.sql.go).
	r.Route("/link", func(r chi.Router) {
		r.Use(requireUser, maxBody)
		r.Post("/", notImplemented("create in-progress link"))
		r.Post("/github", notImplemented("github link"))
		r.Delete("/github", notImplemented("delete github link"))
		r.Get("/github/status", notImplemented("github link status"))
		r.Post("/gmail", notImplemented("gmail link"))
		r.Get("/gmail/status", notImplemented("gmail link status"))
		r.Post("/outlook", notImplemented("outlook link"))
	})

	// --- cursor-api-key -----------------------------------------------------------
	// TODO(port): the cursor_api_keys table exists in schema but has no
	// generated sqlc queries — add them in sqlc/macrodb + regenerate, and use
	// the envelope cipher in mscipher.go (a "cursor-api-key"-purpose
	// sibling of MICROSOFT_TOKEN_MASTER_KEY).
	r.Route("/cursor-api-key", func(r chi.Router) {
		r.Use(requireUser, maxBody)
		r.Get("/", notImplemented("get cursor api key"))
		r.Put("/", notImplemented("put cursor api key"))
		r.Delete("/", notImplemented("delete cursor api key"))
		r.Get("/models", notImplemented("list cursor models"))
		r.Put("/default-model", notImplemented("put cursor default model"))
	})

	// --- codex ----------------------------------------------------------------------
	// TODO(port): codex_connection domain service (device-code OAuth +
	// envelope-encrypted credential store).
	r.Route("/codex", func(r chi.Router) {
		r.Use(requireUser, maxBody)
		r.Get("/", notImplemented("codex connection status"))
		r.Delete("/", notImplemented("codex disconnect"))
		r.Post("/login", notImplemented("codex login start"))
		r.Get("/login/{attempt_id}", notImplemented("codex login poll"))
		r.Delete("/login/{attempt_id}", notImplemented("codex login cancel"))
		r.Get("/environments", notImplemented("codex environments"))
		r.Put("/config", notImplemented("codex configure"))
	})

	// --- github_pull_requests -------------------------------------------------------
	r.Route("/github_pull_requests", func(r chi.Router) {
		r.Use(requireUser)
		// TODO(port): github domain service (PgGithubRepo + github auth
		// client) — enrich PR references with live GitHub data.
		r.Post("/enrich", notImplemented("github pull request enrich"))
	})

	// --- team -------------------------------------------------------------------------
	// TODO(port): teams domain service is a full crate (team repo, invites,
	// CRM settings, Stripe seats). Only GET /team/user is backed by a
	// generated query today; the rest return 501.
	r.Route("/team", func(r chi.Router) {
		r.Use(requireUser, maxBody)
		r.Get("/user", rt.getUserTeams)
		r.Post("/", notImplemented("create team"))
		r.Get("/join/{team_invite_id}", notImplemented("join team"))
		r.Get("/user/invites", notImplemented("user team invites"))
		r.Get("/", notImplemented("get team"))
		r.Patch("/", notImplemented("patch team"))
		r.Delete("/", notImplemented("delete team"))
		r.Patch("/crm", notImplemented("patch team crm"))
		r.Post("/auto-join-domain/toggle", notImplemented("toggle auto join domain"))
		r.Post("/non-admin-invites/toggle", notImplemented("toggle non-admin invites"))
		r.Get("/invites", notImplemented("team invites"))
		r.Post("/invite", notImplemented("invite to team"))
		r.Delete("/join/{team_invite_id}", notImplemented("reject invitation"))
		r.Delete("/remove/{remove_user_id}", notImplemented("remove user from team"))
		r.Delete("/invite/{team_invite_id}", notImplemented("delete team invite"))
	})

	// --- referral ----------------------------------------------------------------------
	// TODO(port): referral domain service (invite send + code lookup, rate
	// limits, Stripe discount client).
	r.Route("/referral", func(r chi.Router) {
		r.Use(requireUser, maxBody)
		r.Post("/send", notImplemented("send referral invite"))
		r.Get("/code", notImplemented("get referral code"))
	})

	// --- gtm-invite ---------------------------------------------------------------------
	// TODO(port): gtm_invite domain service (invite links + Stripe promo).
	r.Route("/gtm-invite", func(r chi.Router) {
		// Public resolve — the only unauthenticated invite endpoint (Rust
		// rate-limits it per-IP; TODO(port) the rate limiter).
		r.Get("/public/{token}", notImplemented("resolve gtm invite"))
		r.With(requireUser).Post("/links", notImplemented("create gtm invite link"))
		r.With(requireUser).Get("/links", notImplemented("list gtm invite links"))
		r.With(requireUser).Delete("/links/{id}", notImplemented("revoke gtm invite link"))
		r.With(requireUser).Post("/redeem", notImplemented("redeem gtm invite"))
		r.With(requireUser).Get("/offer", rt.gtmOffer)
	})

	// --- mobile welcome email ------------------------------------------------------------
	r.With(maxBody).Post("/mobile-welcome-email", rt.mobileWelcomeEmail)

	// --- webhooks ----------------------------------------------------------------------
	// Internal-key authenticated callbacks — in Rust these were FusionAuth
	// webhooks; Casdoor (or another provisioner) can call the same endpoints.
	r.Route("/webhooks/user", func(r chi.Router) {
		r.Use(middleware.RequireInternal(rt.deps.Cfg.InternalAPIKey), maxBody)
		r.Post("/", rt.webhookCreateUser)
		r.Post("/delete", rt.webhookDeleteUser)
		r.Post("/jwt", rt.webhookPopulateJWT)
		r.Post("/name", rt.webhookUpdateName)
		// TODO(port): stripe webhook (subscription bookkeeping).
		r.Post("/stripe", notImplemented("stripe webhook"))
	})

	// --- health ----------------------------------------------------------------------
	if opts.health {
		r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("healthy"))
		})
	}
}

// internalRoutes registers the /internal service-to-service endpoints
// (x-internal-auth-key gated) onto an existing router. It uses With() per
// route rather than Use() so it can merge into a mount the host already
// populated (chi forbids Use() after routes exist).
func (rt *Router) internalRoutes(r chi.Router) {
	requireInternal := middleware.RequireInternal(rt.deps.Cfg.InternalAPIKey)
	// TODO(port): google_access_token needs the gmail link grant +
	// Google token refresh (emaildb link queries).
	maxBody := httpx.MaxBody(httpx.DefaultBodyLimitBytes)
	r.With(requireInternal).Get("/google_access_token", notImplemented("google access token"))
	r.With(requireInternal, maxBody).Post("/get_names", rt.getNamesInternal)
	r.With(requireInternal, maxBody).Get("/get_existing_users", rt.getExistingUsers)
	// TODO(port): remove_link / relocate_inbox_grant /
	// delete_inbox_grant_user mutate emaildb links — port with the
	// email-service link domain.
	r.With(requireInternal).Delete("/remove_link", notImplemented("remove link"))
	r.With(requireInternal).Post("/relocate_inbox_grant", notImplemented("relocate inbox grant"))
	r.With(requireInternal).Delete("/delete_inbox_grant_user", notImplemented("delete inbox grant user"))
}

// notImplemented returns a handler that answers 501 with a pointer at the
// missing piece — used for every route whose domain crate is not ported yet.
func notImplemented(what string) http.HandlerFunc {
	msg := fmt.Sprintf("not implemented: %s", what)
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, msg, http.StatusNotImplemented)
	}
}
