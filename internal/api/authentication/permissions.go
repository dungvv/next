package authentication

import (
	"log/slog"
	"net/http"

	"github.com/macro-inc/macro/internal/api/authentication/middleware"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// permission mirrors model::authentication::permission::Permission.
type permission struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// getPermissions ports GET /permissions — all permissions in the system.
func (rt *Router) getPermissions(w http.ResponseWriter, r *http.Request) {
	rows, err := rt.deps.Q.GetAllPermissions(r.Context())
	if err != nil {
		slog.Error("authentication: get permissions", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "unable to get permissions")
		return
	}
	out := make([]permission, 0, len(rows))
	for _, row := range rows {
		out = append(out, permission{ID: row.ID, Description: row.Description})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// getUserPermissions ports GET /permissions/me — the caller's permission set.
func (rt *Router) getUserPermissions(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	perms, err := userPermissions(r, rt.deps.Q, user.UserID)
	if err != nil {
		slog.Error("authentication: get user permissions", "err", err, "user", user.UserID)
		httpx.Error(w, http.StatusInternalServerError, "unable to get permissions")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, perms)
}

// userPermissions is get_user_permissions: a deduped []string of permission ids.
func userPermissions(r *http.Request, q *macrodb.Queries, userID string) ([]string, error) {
	rows, err := q.GetUserPermissions(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.PermissionID]; ok {
			continue
		}
		seen[row.PermissionID] = struct{}{}
		out = append(out, row.PermissionID)
	}
	return out, nil
}
