package authentication

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/identity"
)

// oauthRedirect ports GET /oauth/redirect — the OAuth callback the provider
// (Casdoor) redirects back to with ?code&state.
//
// Rust flow: exchange code → decode SsoState → store session code for mobile
// → track referral → append signed_up → set cookies → html redirect.
func (rt *Router) oauthRedirect(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	q := r.URL.Query()
	code := q.Get("code")
	if code == "" {
		slog.Warn("authentication: oauth redirect without code",
			"error", q.Get("error"), "error_reason", q.Get("error_reason"),
			"error_description", q.Get("error_description"))
		// Deliberate client-safe message — matches the Rust copy.
		httpx.ErrorJSON(w, http.StatusBadRequest, "Sign-in failed. Please try again or contact support.")
		return
	}
	if d.Identity == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "identity provider not configured")
		return
	}

	// Verify the signed state before exchanging the code: the nonce cookie
	// binds this callback to the browser that ran /login/sso. An absent
	// state is tolerated (provider-initiated login → default redirect,
	// nothing attacker-controlled rides along); a present-but-invalid state
	// is rejected outright.
	var state *ssoState
	if raw := q.Get("state"); raw != "" {
		nonce := d.cookies.OAuthState(r)
		d.cookies.ClearOAuthState(w)
		inner, err := d.Sessions.VerifyOAuthState(raw, nonce)
		if err != nil {
			slog.Warn("authentication: oauth state rejected", "has_nonce_cookie", nonce != "")
			httpx.ErrorJSON(w, http.StatusBadRequest, "invalid oauth state")
			return
		}
		var st ssoState
		if len(inner) > 0 {
			if err := json.Unmarshal(inner, &st); err != nil {
				slog.Error("authentication: decode oauth state", "err", err)
				httpx.ErrorJSON(w, http.StatusBadRequest, "failed to deserialize input")
				return
			}
		}
		state = &st
	}

	grant, err := d.Identity.ExchangeCode(r.Context(), code)
	if err != nil {
		slog.Error("authentication: code grant failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Resolve the provider profile (id_token preferred, userinfo fallback).
	profile, err := rt.resolveProfile(r, grant)
	if err != nil {
		slog.Error("authentication: resolve profile", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !d.signupAllowed(profile.Email) {
		httpx.ErrorJSON(w, http.StatusForbidden, "signup is not allowed for this email")
		return
	}

	// Provision the macro user (JIT — replaces the FusionAuth webhook path).
	mu, isNew, err := d.ensureMacroUser(r.Context(), profile)
	if err != nil {
		slog.Error("authentication: provision user", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	pair, err := d.issueSession(r.Context(), profile, mu)
	if err != nil {
		slog.Error("authentication: issue session", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Decide the redirect target.
	redirectURL, err := rt.redirectURL(r, state, pair)
	if err != nil {
		if errors.Is(err, errDisallowedOriginalURL) {
			httpx.ErrorJSON(w, http.StatusBadRequest, "provided original_url is not allowed")
			return
		}
		slog.Error("authentication: build redirect url", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to build redirect url")
		return
	}

	// TODO(port): referral tracking — state.referral_code is ignored until
	// the referral domain service is ported.

	// Append signed_up=true for brand-new accounts (Rust checks the
	// just-signed-up cache marker set by the create-user webhook; here the
	// JIT provision sets it, and takeUserJustSignedUp also catches the
	// webhook path).
	if isNew || d.takeUserJustSignedUp(r.Context(), profile.Email) {
		appendQueryParam(redirectURL, "signed_up", "true")
	}

	d.cookies.Set(w, pair.AccessToken, pair.RefreshToken)

	// TODO(port): spawn_first_inbox_provision — calls the email service to
	// initialize the primary inbox once the email service is ported.

	writeHTMLRedirect(w, redirectURL)
}

// resolveProfile pulls the user profile out of the token grant.
func (rt *Router) resolveProfile(r *http.Request, grant *identity.TokenGrant) (*identity.Profile, error) {
	if grant.IDToken != "" {
		return rt.deps.Identity.VerifyIDToken(r.Context(), grant.IDToken)
	}
	return rt.deps.Identity.UserInfo(r.Context(), grant.AccessToken)
}

// errDisallowedOriginalURL marks a rejected redirect target.
var errDisallowedOriginalURL = fmt.Errorf("original_url is not allowed")

// redirectURL computes the post-login destination: the validated
// original_url (or the default app URL), with a mobile session code attached
// when the login was initiated with is_mobile.
func (rt *Router) redirectURL(r *http.Request, state *ssoState, pair *identity.TokenPair) (*url.URL, error) {
	d := &rt.deps
	if state == nil {
		u, err := url.Parse(d.defaultRedirectURL())
		return u, err
	}
	var u *url.URL
	var err error
	if state.OriginalURL != nil && *state.OriginalURL != "" {
		u, err = url.Parse(*state.OriginalURL)
		if err != nil {
			return nil, err
		}
		// Re-validate: state is client-visible and could be forged on the
		// way back through the provider.
		if !isAllowedOriginalURL(u, d.allowedOriginalURLHosts()) {
			slog.Error("authentication: original_url in oauth state not allowed", "url", redactURL(u))
			return nil, errDisallowedOriginalURL
		}
	} else {
		u, err = url.Parse(d.defaultRedirectURL())
		if err != nil {
			return nil, err
		}
	}
	if state.IsMobile {
		code, err := generateSessionCode()
		if err != nil {
			return nil, err
		}
		if err := d.setMobileLoginSession(r.Context(), code, pair.RefreshToken); err != nil {
			return nil, fmt.Errorf("unable to store session code: %w", err)
		}
		// Strip any pre-existing token param, then append the fresh one.
		q := u.Query()
		q.Del("token")
		q.Set("token", code)
		u.RawQuery = q.Encode()
	}
	return u, nil
}

func appendQueryParam(u *url.URL, k, v string) {
	q := u.Query()
	q.Set(k, v)
	u.RawQuery = q.Encode()
}

// writeHTMLRedirect ports the maud html_redirect — a meta-refresh page,
// because the response may carry cookies a 30x could drop on some clients.
func writeHTMLRedirect(w http.ResponseWriter, u *url.URL) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Redirect</title><meta http-equiv="refresh" content="0;url=%s"></head></html>`, html.EscapeString(u.String()))
}
