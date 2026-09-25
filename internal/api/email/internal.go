package email

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// notImplemented returns a handler that responds 501 for surface area the Go
// port deliberately defers (filters, thread histories, ...).
func (d *Deps) notImplemented(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.ErrorJSON(w, http.StatusNotImplemented, name+" not implemented")
	}
}

// internalGetMessage handles GET /internal/messages/{id} — same payload as the
// user-facing GET but without link scoping (callers are internal services).
func (d *Deps) internalGetMessage(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid message id")
		return
	}
	ctx := r.Context()
	row, err := d.q().GetParsedMessageById(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch message")
		return
	}
	msgs := []parsedMsg{parsedFromByIdRow(row)}
	aux, err := d.fetchAux(ctx, msgs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch message details")
		return
	}
	a := aux[msgs[0].ID]
	httpx.WriteJSON(w, http.StatusOK, apiMessage(msgs[0], a.from, a.to, a.cc, a.bcc, a.labels, a.atts, a.drafts, a.fwd, a.sendTime))
}

// internalGetMessagesBatch handles POST /internal/messages/batch.
func (d *Deps) internalGetMessagesBatch(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	var ids []uuid.UUID
	if !httpx.DecodeJSON(w, r, &ids) {
		return
	}
	if len(ids) == 0 {
		httpx.ErrorJSON(w, http.StatusBadRequest, "must include at least one message ID")
		return
	}
	pgIDs := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		pgIDs = append(pgIDs, pgUUID(id))
	}
	ctx := r.Context()
	rows, err := d.q().GetParsedMessagesByIdBatch(ctx, pgIDs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch messages")
		return
	}
	msgs := make([]parsedMsg, 0, len(rows))
	for _, row := range rows {
		msgs = append(msgs, parsedFromBatchRow(row))
	}
	aux, err := d.fetchAux(ctx, msgs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch message details")
		return
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		a := aux[m.ID]
		out = append(out, apiMessage(m, a.from, a.to, a.cc, a.bcc, a.labels, a.atts, a.drafts, a.fwd, a.sendTime))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// internalGetSenders handles POST /internal/messages/senders — returns the
// sender contact for each requested message id.
func (d *Deps) internalGetSenders(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	var ids []uuid.UUID
	if !httpx.DecodeJSON(w, r, &ids) {
		return
	}
	pgIDs := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		pgIDs = append(pgIDs, pgUUID(id))
	}
	rows, err := d.q().FetchSendersByMessageIds(r.Context(), pgIDs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch senders")
		return
	}
	out := make(map[uuid.UUID]ContactInfo, len(rows))
	for _, s := range rows {
		out[uuidFromPg(s.MessageID)] = contactFromSender(s, pgtype.Text{})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// internalDeleteUser handles DELETE /internal/delete_user/{id} — tears down
// every email link owned by the (fusionauth) user id: removes stored gmail
// tokens, publishes link-manager teardown jobs, and deletes the link rows.
func (d *Deps) internalDeleteUser(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	userID := chi.URLParam(r, "id")
	if userID == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "missing user id")
		return
	}
	ctx := r.Context()
	links, err := d.q().FetchLinksByFusionauthUserId(ctx, userID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch links")
		return
	}
	for _, l := range links {
		linkID := uuidFromPg(l.ID)
		if gc := d.gmailClient(ctx, linkID); gc != nil {
			_ = gc.Stop(ctx)
		}
		if d.Tokens != nil {
			if err := d.Tokens.Delete(ctx, linkID); err != nil {
				slog.Warn("email: token delete failed", "link_id", linkID, "err", err)
			}
		}
		d.publishJobBestEffort(ctx, subjLinkManager, "link_manager.delete", LinkManagerDeleteJob{
			LinkID:      linkID,
			Email:       l.EmailAddress,
			AuthID:      l.FusionauthUserID,
			DeletedByID: c.UserID,
		})
		if err := d.q().DeleteLinkById(ctx, l.ID); err != nil {
			slog.Warn("email: link delete failed", "link_id", linkID, "err", err)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// internalGetThreadOwner handles GET /internal/threads/{id}/owner — returns
// the macro_id owning the thread (used by other services for authz checks).
func (d *Deps) internalGetThreadOwner(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	macroID, err := d.q().GetMacroIdFromThreadId(r.Context(), pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "thread not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread owner")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"macro_id": macroID})
}
