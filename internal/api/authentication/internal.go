package authentication

import (
	"log/slog"
	"net/http"

	"github.com/macro-inc/macro/internal/api/httpx"
)

// getNamesInternal ports POST /internal/get_names — identical behavior to the
// external /user/get_names, gated on the internal API key instead of a user
// token (InternalOnly extractor in Rust).
func (rt *Router) getNamesInternal(w http.ResponseWriter, r *http.Request) {
	rt.getNames(w, r)
}

// getExistingUsers ports GET /internal/get_existing_users: given
// {user_ids:[…]} returns {existing_user_ids:[…]} — the subset that exists as
// "User" rows.
func (rt *Router) getExistingUsers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserIDs []string `json:"user_ids"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ids := make([]string, 0, len(req.UserIDs))
	for _, raw := range req.UserIDs {
		id, ok := validMacroUserID(raw)
		if !ok {
			httpx.ErrorJSON(w, http.StatusBadRequest, "invalid macro user id provided")
			return
		}
		ids = append(ids, id)
	}
	existing, err := rt.deps.Q.GetExistingUsers(r.Context(), ids)
	if err != nil {
		slog.Error("authentication: get existing users", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		ExistingUserIDs []string `json:"existing_user_ids"`
	}{ExistingUserIDs: existing})
}
