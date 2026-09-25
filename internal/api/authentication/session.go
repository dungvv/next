package authentication

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/macro-inc/macro/internal/api/authentication/middleware"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/identity"
)

// sessionCreate ports POST /session: stash the caller's refresh token under a
// one-time session code in Valkey (mbl_login:<code>, 5 min) so a mobile app
// can pick it up via /session/login/{code}.
func (rt *Router) sessionCreate(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	refresh := middleware.RefreshToken(r, d.cookies)
	if refresh == "" {
		httpx.Error(w, http.StatusBadRequest, "no refresh token")
		return
	}
	code, err := generateSessionCode()
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := d.setMobileLoginSession(r.Context(), code, refresh); err != nil {
		slog.Error("authentication: store session code", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to store session code")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// sessionLogin ports GET /session/login/{session_code}: redeem a session code
// for a fresh token pair. The stored refresh token is validated (signature +
// expiry + type) then a new pair is issued — the Rust handler ran the
// provider refresh grant here; ours is stateless.
func (rt *Router) sessionLogin(w http.ResponseWriter, r *http.Request) {
	d := &rt.deps
	code := chi.URLParam(r, "session_code")
	refresh, err := d.getMobileLoginSession(r.Context(), code)
	if err != nil {
		slog.Error("authentication: get mobile login session", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get session code")
		return
	}
	if refresh == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid session code")
		return
	}
	claims, err := d.consumeRefresh(r.Context(), refresh)
	if err != nil {
		slog.Error("authentication: session refresh", "err", err)
		if errors.Is(err, identity.ErrTokenExpired) ||
			errors.Is(err, identity.ErrRefreshTokenReused) {
			httpx.ErrorJSON(w, http.StatusBadRequest, "invalid session code")
			return
		}
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to refresh token")
		return
	}
	pair, err := rt.reissueTokens(r, claims.Subject)
	if err != nil {
		slog.Error("authentication: session reissue", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to refresh token")
		return
	}
	d.cookies.Set(w, pair.AccessToken, pair.RefreshToken)
	httpx.WriteJSON(w, http.StatusOK, pair)
}
