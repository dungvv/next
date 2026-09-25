package authentication

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/authentication/middleware"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/identity"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// jwtRefresh ports POST /jwt/refresh: if the access token is still valid the
// same pair is returned; if it is expired the refresh token is exchanged for
// a fresh pair. Tokens come from cookies or headers
// (middleware/extract_tokens.rs equivalent).
func (rt *Router) jwtRefresh(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	access := middleware.AccessToken(r, d.cookies)
	refresh := middleware.RefreshToken(r, d.cookies)
	if access == "" {
		httpx.Error(w, http.StatusBadRequest, "no access token to refresh")
		return
	}
	if refresh == "" {
		httpx.Error(w, http.StatusBadRequest, "no refresh token to refresh")
		return
	}

	_, err := d.Validator.ValidateAccessToken(access)
	if err == nil {
		// Still valid — echo the pair back (Rust behavior).
		httpx.WriteJSON(w, http.StatusOK, identity.TokenPair{AccessToken: access, RefreshToken: refresh})
		return
	}
	if !errors.Is(err, identity.ErrTokenExpired) {
		slog.Error("authentication: refresh decode jwt", "err", err)
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Verify the refresh token and rotate it: the presented token's jti is
	// consumed (a replay gets 401) and the response carries a fresh pair
	// (FusionAuth rotate-on-use semantics).
	claims, err := d.consumeRefresh(r.Context(), refresh)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrTokenExpired):
			httpx.Error(w, http.StatusUnauthorized, "refresh token expired")
		case errors.Is(err, identity.ErrRefreshTokenReused):
			slog.Warn("authentication: refresh token reuse")
			httpx.Error(w, http.StatusUnauthorized, "refresh token already used")
		default:
			slog.Error("authentication: invalid refresh token", "err", err)
			httpx.Error(w, http.StatusBadRequest, "invalid refresh token")
		}
		return
	}

	pair, err := rt.reissueTokens(r, claims.Subject)
	if err != nil {
		slog.Error("authentication: reissue tokens", "err", err, "user", claims.Subject)
		httpx.Error(w, http.StatusInternalServerError, "unable to refresh token")
		return
	}
	d.cookies.Set(w, pair.AccessToken, pair.RefreshToken)
	httpx.WriteJSON(w, http.StatusOK, pair)
}

// reissueTokens loads the user and mints a fresh pair. The macro user id is
// "macro|<email>" — the email part keys the profile lookup.
func (rt *Router) reissueTokens(r *http.Request, macroUserID string) (*identity.TokenPair, error) {
	email := strings.TrimPrefix(macroUserID, "macro|")
	mu, err := rt.deps.lookupMacroUser(r.Context(), email)
	if err != nil {
		return nil, err
	}
	return rt.deps.issueSession(r.Context(), &identity.Profile{
		Email:          email,
		ProviderUserID: mu.RootMacroID,
	}, mu)
}

// macroAPIToken ports GET /jwt/macro_api_token: signs a long-lived RS256
// token (kid="macro") for the authenticated user.
func (rt *Router) macroAPIToken(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	user, _ := middleware.FromContext(r.Context())

	if d.APITokenKey == "" {
		httpx.Error(w, http.StatusServiceUnavailable, "macro-api-token signing not configured")
		return
	}

	email := r.URL.Query().Get("email")
	if email != "" {
		if dec, err := url.QueryUnescape(email); err == nil {
			email = dec
		}
	} else if user.UserID == "" {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	} else {
		email = strings.ReplaceAll(user.UserID, "macro|", "")
	}

	// The Rust query keys on macro_user_id (the root_macro_id UUID stored in
	// User.macro_user_id) + email.
	macroUUID := pgtype.UUID{}
	if user.ProviderUserID != "" {
		if u, err := parseUUID(user.ProviderUserID); err == nil {
			macroUUID = u
		}
	}
	row, err := d.Q.GetUserProfileByFusionauthUserIdAndEmail(r.Context(),
		macrodb.GetUserProfileByFusionauthUserIdAndEmailParams{
			MacroUserID: macroUUID,
			Lower:       email,
		})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusUnauthorized, "no access to this profile")
		return
	}
	if err != nil {
		slog.Error("authentication: get user profile", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "unable to get user profile")
		return
	}

	var orgID *int64
	if row.OrganizationID.Valid {
		org := int64(row.OrganizationID.Int32)
		orgID = &org
	}
	tok, err := identity.EncodeMacroAPIToken(
		d.APITokenKey, d.APITokenIssuer,
		user.ProviderUserID, row.ID, orgID, d.APITokenTTL,
	)
	if err != nil {
		slog.Error("authentication: encode macro-api-token", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "unable to encode macro-api-token")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		MacroAPIToken string `json:"macro_api_token"`
	}{MacroAPIToken: tok})
}

func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return u, err
	}
	return u, nil
}
