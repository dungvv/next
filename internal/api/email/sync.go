package email

import (
	"net/http"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// disableSync handles DELETE /email/sync — sets is_sync_active=false on the
// caller's inbox and enqueues a gmail watch stop.
func (d *Deps) disableSync(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	if err := d.q().UpdateLinkSyncStatus(r.Context(), emaildb.UpdateLinkSyncStatusParams{
		ID: pgUUID(link.ID), IsSyncActive: false,
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to disable sync")
		return
	}
	if gc := d.gmailClient(r.Context(), link.ID); gc != nil {
		_ = gc.Stop(r.Context())
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}
