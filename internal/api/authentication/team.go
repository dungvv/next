package authentication

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/authentication/middleware"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// team mirrors the serialized teams::domain::model::Team. The generated
// GetUserTeams query only selects id/name/owner_id, so the remaining fields
// are emitted as zero values until the teams domain service is ported.
//
// TODO(port): slug, crm_enabled, auto_join_domain, enterprise,
// allow_non_admin_invites, default_link_share come from the teams repo (the
// team_crm_settings join) — wire them when the teams crate is ported.
type team struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	Slug                string  `json:"slug"`
	OwnerID             string  `json:"owner_id"`
	CrmEnabled          bool    `json:"crm_enabled"`
	AutoJoinDomain      *string `json:"auto_join_domain"`
	Enterprise          bool    `json:"enterprise"`
	AllowNonAdminInvite bool    `json:"allow_non_admin_invites"`
	DefaultLinkShare    *string `json:"default_link_share"`
}

// getUserTeams ports GET /team/user — the teams the caller belongs to.
func (rt *Router) getUserTeams(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.FromContext(r.Context())
	rows, err := rt.deps.Q.GetUserTeams(r.Context(), user.UserID)
	if err != nil {
		slog.Error("authentication: get user teams", "err", err, "user", user.UserID)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]team, 0, len(rows))
	for _, row := range rows {
		out = append(out, team{
			ID:      uuid.UUID(row.ID.Bytes).String(),
			Name:    row.Name,
			OwnerID: row.OwnerID,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
