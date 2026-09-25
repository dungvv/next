package authentication

import (
	"net/http"

	"github.com/macro-inc/macro/internal/api/authentication/middleware"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// logout ports GET+POST /logout: revoke the presented refresh token (its jti
// is deleted from the store, so it can never be refreshed again) and expire
// both auth cookies. The Rust handler also called FusionAuth's per-tenant
// logout; our access token stays valid until its (short) expiry — the
// provider SSO session (Casdoor) is only ended when the browser hits
// LogoutURL, which the frontend can do itself.
//
// TODO(port): access-token revocation — add a Valkey denylist (jti) if
// logout must invalidate in-flight access tokens before expiry.
func (rt *Router) logout(w http.ResponseWriter, r *http.Request) {
	rt.deps.revokeRefresh(r.Context(), middleware.RefreshToken(r, rt.deps.cookies))
	rt.deps.cookies.ClearAccess(w)
	rt.deps.cookies.ClearRefresh(w)
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}
