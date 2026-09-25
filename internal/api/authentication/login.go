package authentication

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/identity"
)

// ssoState mirrors login::sso::SsoState — the JSON blob round-tripped through
// the provider's `state` param.
type ssoState struct {
	OriginalURL  *string `json:"original_url"`
	IsMobile     bool    `json:"is_mobile"`
	ReferralCode *string `json:"referral_code"`
}

// loginSSO ports GET /login/sso: build the provider authorize URL and
// redirect. idp_name/idp_id map onto Casdoor's `provider` hint.
func (rt *Router) loginSSO(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	if d.Identity == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "identity provider not configured")
		return
	}
	q := r.URL.Query()
	idpName := q.Get("idp_name")
	idpID := q.Get("idp_id")
	loginHint := q.Get("login_hint")
	isMobile := q.Get("is_mobile") == "true"
	referralCode := q.Get("referral_code")

	// The frontend double-encodes original_url (serde UrlEncoded<Url> decodes
	// once); Query() already decoded once, so unescape again here.
	var originalURL *url.URL
	if raw := q.Get("original_url"); raw != "" {
		decoded, err := url.QueryUnescape(raw)
		if err != nil {
			decoded = raw
		}
		u, err := url.Parse(decoded)
		if err != nil || !isAllowedOriginalURL(u, d.allowedOriginalURLHosts()) {
			slog.Error("authentication: original_url is not allowed", "url", redactURL(u))
			httpx.ErrorJSON(w, http.StatusBadRequest, "provided original_url is not allowed")
			return
		}
		originalURL = u
	}

	if idpName == "" && idpID == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "idp_name or idp_id need to be provided")
		return
	}
	// Casdoor resolves providers by name; prefer idp_name, fall back to
	// idp_id (which a Casdoor migration maps to the provider name).
	provider := idpName
	if provider == "" {
		provider = idpID
	}

	// Always emit state now: it carries the login params plus an HMAC-signed
	// nonce that binds the callback to this browser (login-CSRF defense —
	// standard OIDC practice; the Rust service left state unsigned).
	st := ssoState{IsMobile: isMobile}
	if originalURL != nil {
		s := originalURL.String()
		st.OriginalURL = &s
	}
	if referralCode != "" {
		st.ReferralCode = &referralCode
	}
	inner, err := json.Marshal(st)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "unable to serialize state into string")
		return
	}
	nonce, err := newNonce()
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	stateStr, err := d.Sessions.SignOAuthState(inner, nonce, identity.OAuthStateTTL)
	if err != nil {
		slog.Error("authentication: sign oauth state", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to construct authorize url")
		return
	}
	d.cookies.SetOAuthState(w, nonce, identity.OAuthStateTTL)

	ssoURL, err := d.Identity.AuthorizeURL(identity.AuthorizeRequest{
		Provider:  provider,
		LoginHint: loginHint,
		State:     stateStr,
	})
	if err != nil {
		slog.Error("authentication: build authorize url", "err", err)
		httpx.ErrorJSON(w, http.StatusBadRequest, "unable to construct authorize url")
		return
	}
	slog.Info("authentication: sso redirect", "url", ssoURL)
	// Rust uses Redirect::temporary → 307.
	http.Redirect(w, r, ssoURL, http.StatusTemporaryRedirect)
}

// redactURL strips query/fragment/userinfo for logging (login::sso port).
func redactURL(u *url.URL) string {
	if u == nil {
		return "<nil>"
	}
	c := *u
	c.RawQuery = ""
	c.Fragment = ""
	c.User = nil
	return c.String()
}

// loginPassword ports POST /login/password via the provider password grant.
// Rust semantics: an unregistered email is registered then the login retried.
func (rt *Router) loginPassword(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	if d.Identity == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "identity provider not configured")
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !isValidEmail(email) {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid email")
		return
	}

	grant, err := d.Identity.PasswordLogin(r.Context(), email, req.Password)
	if errors.Is(err, identity.ErrInvalidCredentials) && d.Admin != nil {
		// Port of the FusionAuth UserNotRegistered branch — with one
		// correction: FusionAuth registered an *existing, verified* identity
		// and rejected unverified ones with UserNotVerified. Creating the
		// provider account here leaves it unverified, so the caller can never
		// turn an arbitrary email into a live session; login completes only
		// after the provider-side email verification.
		existing, gerr := d.Admin.AdminGetUserByEmail(r.Context(), email)
		switch {
		case gerr != nil:
			slog.Error("authentication: lookup provider user", "err", gerr)
		case existing == nil:
			if !d.signupAllowed(email) {
				httpx.ErrorJSON(w, http.StatusForbidden, "signup is not allowed for this email")
				return
			}
			if cerr := d.Admin.AdminCreateUser(r.Context(),
				&identity.Profile{Email: email, Name: email}, req.Password); cerr != nil {
				slog.Error("authentication: register user", "err", cerr)
				httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to register user")
				return
			}
			// Best effort: the user verifies via the provider's own flow.
			if verr := d.Admin.AdminSendVerificationCode(r.Context(), email); verr != nil {
				slog.Warn("authentication: send verification code failed", "err", verr)
			}
			// Rust's post-register re-login failed with UserNotVerified —
			// the same 401 without burning a second grant call.
			httpx.ErrorJSON(w, http.StatusUnauthorized, "user has not verified their primary email")
			return
		case !existing.EmailVerified:
			if verr := d.Admin.AdminSendVerificationCode(r.Context(), email); verr != nil {
				slog.Warn("authentication: resend verification code failed", "err", verr)
			}
			httpx.ErrorJSON(w, http.StatusUnauthorized, "user has not verified their primary email")
			return
		}
	}
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCredentials) {
			httpx.ErrorJSON(w, http.StatusUnauthorized, "unable to login user")
			return
		}
		slog.Error("authentication: password login", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to login user")
		return
	}

	rt.completeLogin(w, r, email, grant)
}

// completeLogin turns a provider grant into a session: resolve/provision the
// macro user, issue tokens, set cookies, write the UserTokensResponse.
func (rt *Router) completeLogin(w http.ResponseWriter, r *http.Request, email string, grant *identity.TokenGrant) bool {
	d := &rt.deps
	profile := &identity.Profile{Email: email}
	if grant.IDToken != "" {
		p, err := d.Identity.VerifyIDToken(r.Context(), grant.IDToken)
		if err != nil {
			slog.Error("authentication: verify id token", "err", err)
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to verify identity")
			return false
		}
		profile = p
	} else if grant.AccessToken != "" {
		p, err := d.Identity.UserInfo(r.Context(), grant.AccessToken)
		if err != nil {
			slog.Error("authentication: userinfo", "err", err)
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch userinfo")
			return false
		}
		profile = p
	}
	if profile.Email == "" {
		profile.Email = email
	}
	if !d.signupAllowed(profile.Email) {
		httpx.ErrorJSON(w, http.StatusForbidden, "signup is not allowed for this email")
		return false
	}
	// The provider account's email must be verified before a session is
	// issued (FusionAuth UserNotVerified parity). SSO-verified profiles skip
	// the admin lookup.
	if err := d.requireVerifiedProviderEmail(r.Context(), profile); err != nil {
		if errors.Is(err, errProviderEmailUnverified) {
			httpx.ErrorJSON(w, http.StatusUnauthorized, "user has not verified their primary email")
		} else {
			slog.Error("authentication: check provider verification", "err", err)
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to login user")
		}
		return false
	}
	mu, _, err := d.ensureMacroUser(r.Context(), profile)
	if err != nil {
		slog.Error("authentication: ensure macro user", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to provision user")
		return false
	}
	pair, err := d.issueSession(r.Context(), profile, mu)
	if err != nil {
		slog.Error("authentication: issue session", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to issue session")
		return false
	}
	d.cookies.Set(w, pair.AccessToken, pair.RefreshToken)
	httpx.WriteJSON(w, http.StatusOK, pair)
	return true
}

func isValidEmail(s string) bool {
	at := strings.LastIndex(s, "@")
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t\n")
}

// newNonce mints a random 128-bit hex nonce for the oauth-state cookie.
func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
