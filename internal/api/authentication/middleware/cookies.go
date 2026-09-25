package middleware

import (
	"net/http"
	"time"
)

// Cookie names and header — macro_auth::constant.
const (
	accessTokenCookie  = "macro-access-token"
	refreshTokenCookie = "macro-refresh-token"
	// oauthStateCookie carries the nonce that binds an OAuth authorization
	// response to the browser that started the flow (login-CSRF defense).
	oauthStateCookie = "macro-oauth-state"
	// HeaderRefreshToken mirrors MACRO_REFRESH_TOKEN_HEADER.
	HeaderRefreshToken = "x-macro-refresh-token"
)

// Cookies bakes the environment-scoped auth cookies, mirroring
// api::utils::create_access_token_cookie / create_refresh_token_cookie.
type Cookies struct {
	env    string // "production" | "develop" | "local" (anything else → local)
	domain string // cookie domain; empty = host-only
}

// NewCookies builds the cookie manager for an environment. env uses the
// service ENVIRONMENT spelling ("production"/"develop"/"local"); prod/dev
// default to the macro.com domain unless overridden.
func NewCookies(env, domainOverride string) *Cookies {
	c := &Cookies{env: normalizeEnv(env)}
	switch c.env {
	case "production", "develop":
		c.domain = "macro.com"
	}
	if domainOverride != "" {
		c.domain = domainOverride
	}
	return c
}

func normalizeEnv(env string) string {
	switch env {
	case "production", "prod":
		return "production"
	case "develop", "dev", "staging":
		return "develop"
	default:
		return "local"
	}
}

// AccessTokenName is the environment-prefixed access cookie name.
func (c *Cookies) AccessTokenName() string {
	switch c.env {
	case "production":
		return accessTokenCookie
	case "develop":
		return "dev-" + accessTokenCookie
	default:
		return "local-" + accessTokenCookie
	}
}

// RefreshTokenName is the environment-prefixed refresh cookie name.
func (c *Cookies) RefreshTokenName() string {
	switch c.env {
	case "production":
		return refreshTokenCookie
	case "develop":
		return "dev-" + refreshTokenCookie
	default:
		return "local-" + refreshTokenCookie
	}
}

func (c *Cookies) sameSite() http.SameSite {
	if c.env == "production" {
		return http.SameSiteStrictMode
	}
	return http.SameSiteNoneMode
}

func (c *Cookies) bake(name, value string) *http.Cookie {
	cookie := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: c.sameSite(),
		Expires:  time.Now().Add(365 * 24 * time.Hour),
	}
	if c.domain != "" {
		cookie.Domain = c.domain
	}
	return cookie
}

// AccessTokenCookie builds the macro-access-token cookie.
func (c *Cookies) AccessTokenCookie(token string) *http.Cookie {
	return c.bake(c.AccessTokenName(), token)
}

// RefreshTokenCookie builds the macro-refresh-token cookie.
func (c *Cookies) RefreshTokenCookie(token string) *http.Cookie {
	return c.bake(c.RefreshTokenName(), token)
}

// ClearAccess writes an expired access-token cookie (logout / delete user).
func (c *Cookies) ClearAccess(w http.ResponseWriter) {
	cookie := c.AccessTokenCookie("")
	cookie.Expires = time.Now()
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}

// ClearRefresh writes an expired refresh-token cookie.
func (c *Cookies) ClearRefresh(w http.ResponseWriter) {
	cookie := c.RefreshTokenCookie("")
	cookie.Expires = time.Now()
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}

// Set writes both auth cookies.
func (c *Cookies) Set(w http.ResponseWriter, access, refresh string) {
	http.SetCookie(w, c.AccessTokenCookie(access))
	http.SetCookie(w, c.RefreshTokenCookie(refresh))
}

// OAuthStateName is the environment-prefixed oauth-state cookie name.
func (c *Cookies) OAuthStateName() string {
	switch c.env {
	case "production":
		return oauthStateCookie
	case "develop":
		return "dev-" + oauthStateCookie
	default:
		return "local-" + oauthStateCookie
	}
}

// SetOAuthState writes the short-lived nonce cookie the /oauth/redirect
// callback verifies the signed state against. SameSite=Lax is required (not
// None/Strict): the cookie must ride the top-level GET redirect back from
// the provider.
func (c *Cookies) SetOAuthState(w http.ResponseWriter, nonce string, maxAge time.Duration) {
	cookie := c.bake(c.OAuthStateName(), nonce)
	cookie.SameSite = http.SameSiteLaxMode
	cookie.Expires = time.Now().Add(maxAge)
	cookie.MaxAge = int(maxAge.Seconds())
	http.SetCookie(w, cookie)
}

// OAuthState reads the nonce cookie.
func (c *Cookies) OAuthState(r *http.Request) string {
	if ck, err := r.Cookie(c.OAuthStateName()); err == nil {
		return ck.Value
	}
	return ""
}

// ClearOAuthState expires the nonce cookie after the callback consumed it.
func (c *Cookies) ClearOAuthState(w http.ResponseWriter) {
	cookie := c.bake(c.OAuthStateName(), "")
	cookie.SameSite = http.SameSiteLaxMode
	cookie.Expires = time.Now()
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}
