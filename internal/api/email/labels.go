package email

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// listLabels handles GET /email/labels.
func (d *Deps) listLabels(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	rows, err := d.q().FetchLabelsByLinkId(r.Context(), pgUUID(link.ID))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch labels")
		return
	}
	labels := make([]Label, 0, len(rows))
	for _, l := range rows {
		labels = append(labels, labelFromDB(l))
	}
	httpx.WriteJSON(w, http.StatusOK, ListLabelsResponse{Labels: labels})
}

// createLabel handles POST /email/labels {name}. The provider label is
// created asynchronously through the gmail_ops queue when configured; the
// local label uses a deterministic provider id placeholder.
func (d *Deps) createLabel(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req CreateLabelRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "label name is required")
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	// Reject name collisions for the link.
	existing, err := d.q().FetchLabelsByLinkId(ctx, pgUUID(link.ID))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch labels")
		return
	}
	for _, l := range existing {
		if l.Name == req.Name {
			httpx.ErrorJSON(w, http.StatusBadRequest, "label with this name already exists")
			return
		}
	}
	// If we have a Gmail client, create the provider label first so we can
	// store its real provider_label_id; otherwise fall back to a placeholder
	// and let sync reconcile it.
	providerID := "macro_" + uuid.NewString()
	if gc := d.gmailClient(ctx, link.ID); gc != nil {
		gl, err := gc.CreateLabel(ctx, req.Name)
		if err == nil && gl != nil && gl.ID != "" {
			providerID = gl.ID
		}
	}
	row, err := d.q().InsertLabel(ctx, emaildb.InsertLabelParams{
		ID:                    pgUUID(uuid.New()),
		LinkID:                pgUUID(link.ID),
		ProviderLabelID:       providerID,
		Name:                  req.Name,
		MessageListVisibility: emaildb.EmailMessageListVisibilityEnumShow,
		LabelListVisibility:   emaildb.EmailLabelListVisibilityEnumLabelShow,
		Type:                  emaildb.EmailLabelTypeEnumUser,
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create label")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, CreateLabelResponse{Label: labelFromDB(row)})
}

// deleteLabel handles DELETE /email/labels/{id}. Local delete is
// authoritative; provider delete is enqueued best-effort.
func (d *Deps) deleteLabel(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid label id")
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	lbl, err := d.q().FetchLabelById(ctx, emaildb.FetchLabelByIdParams{
		ID: pgUUID(id), LinkID: pgUUID(link.ID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "label not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch label")
		return
	}
	if lbl.Type == emaildb.EmailLabelTypeEnumSystem {
		httpx.ErrorJSON(w, http.StatusBadRequest, "cannot delete system label")
		return
	}
	if err := d.q().DeleteLabelById2(ctx, emaildb.DeleteLabelById2Params{
		ID: pgUUID(id), LinkID: pgUUID(link.ID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to delete label")
		return
	}
	d.publishJobBestEffort(ctx, subjGmailOps, "gmail_ops.delete_label",
		gmailOps(link.ID, "delete_label", map[string]any{
			"provider_label_id": lbl.ProviderLabelID,
		}))
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}
