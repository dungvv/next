package email

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

const batchMessageLimit = 100
const batchUpdateMessageLimit = 10

// messageAux bundles the per-message related rows fetched in bulk.
type messageAux struct {
	from        *ContactInfo
	to, cc, bcc []ContactInfo
	labels      []MessageLabel
	atts        []MessageAttachment
	drafts      []AttachmentDraft
	fwd         []AttachmentForwarded
	sendTime    *time.Time
}

// fetchAux bulk-loads recipients/senders/labels/attachments for parsed msgs.
func (d *Deps) fetchAux(ctx context.Context, msgs []parsedMsg) (map[uuid.UUID]*messageAux, error) {
	ids := make([]pgtype.UUID, 0, len(msgs))
	byID := make(map[uuid.UUID]*parsedMsg, len(msgs))
	for i := range msgs {
		m := msgs[i]
		ids = append(ids, pgUUID(m.ID))
		byID[m.ID] = &msgs[i]
	}
	aux := make(map[uuid.UUID]*messageAux, len(msgs))
	for i := range msgs {
		aux[msgs[i].ID] = &messageAux{}
	}
	if len(ids) == 0 {
		return aux, nil
	}

	// recipients
	recips, err := d.q().FetchDbRecipientsInBulk(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range recips {
		mid := uuidFromPg(r.MessageID)
		c := contactFromRecipient(r)
		switch r.RecipientType {
		case emaildb.EmailRecipientTypeTO:
			aux[mid].to = append(aux[mid].to, c)
		case emaildb.EmailRecipientTypeCC:
			aux[mid].cc = append(aux[mid].cc, c)
		case emaildb.EmailRecipientTypeBCC:
			aux[mid].bcc = append(aux[mid].bcc, c)
		}
	}

	// senders
	senders, err := d.q().FetchSendersByMessageIds(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, s := range senders {
		mid := uuidFromPg(s.MessageID)
		var fromName pgtype.Text
		if pm, ok := byID[mid]; ok && pm.FromName != nil {
			fromName = pgtype.Text{String: *pm.FromName, Valid: true}
		}
		c := contactFromSender(s, fromName)
		aux[mid].from = &c
	}

	// labels
	lbls, err := d.q().FetchMessageLabelsInBulk(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, l := range lbls {
		mid := uuidFromPg(l.MessageID)
		aux[mid].labels = append(aux[mid].labels, msgLabelFromBulk(l))
	}

	// provider attachments
	atts, err := d.q().FetchDbAttachmentsInBulk(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, a := range atts {
		mid := uuidFromPg(a.MessageID)
		aux[mid].atts = append(aux[mid].atts, attachmentFromBulk(a))
	}

	// draft attachments (only for draft messages)
	var draftIDs []pgtype.UUID
	for _, m := range msgs {
		if m.IsDraft {
			draftIDs = append(draftIDs, pgUUID(m.ID))
		}
	}
	if len(draftIDs) > 0 {
		rows, err := d.q().FetchDbDraftAttachmentsInBulk(ctx, draftIDs)
		if err != nil {
			return nil, err
		}
		for _, a := range rows {
			mid := uuidFromPg(a.DraftID)
			aux[mid].drafts = append(aux[mid].drafts, draftAttachmentFromBulk(a))
		}
		// forwarded attachments on drafts (need link scope)
		for _, m := range msgs {
			if !m.IsDraft {
				continue
			}
			frows, err := d.q().FetchForwardedAttachmentsByDraftId(ctx, emaildb.FetchForwardedAttachmentsByDraftIdParams{
				MessageID: pgUUID(m.ID),
				LinkID:    pgUUID(m.LinkID),
			})
			if err != nil {
				return nil, err
			}
			for _, f := range frows {
				aux[m.ID].fwd = append(aux[m.ID].fwd, fwdAttachmentFromRow(f))
			}
		}
	}

	// scheduled send times
	sched, err := d.q().FetchScheduledMessagesInBulk(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, s := range sched {
		mid := uuidFromPg(s.MessageID)
		aux[mid].sendTime = tsPtrFromPg(s.SendTime)
	}

	return aux, nil
}

// ownedLinkForMessage resolves the caller's link owning a message.
func (d *Deps) ownedLinkForMessage(ctx context.Context, msgID uuid.UUID, macroID string) (linkRow, error) {
	row, err := d.q().FetchOwnedLinkForMessage(ctx, emaildb.FetchOwnedLinkForMessageParams{
		ID:      pgUUID(msgID),
		MacroID: macroID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return linkRow{}, errLinkNotFound
	}
	if err != nil {
		return linkRow{}, err
	}
	return linkFromOwnedForMessage(row), nil
}

func (d *Deps) ownedLinkForThread(ctx context.Context, threadID uuid.UUID, macroID string) (linkRow, error) {
	row, err := d.q().FetchOwnedLinkForThread(ctx, emaildb.FetchOwnedLinkForThreadParams{
		ID:      pgUUID(threadID),
		MacroID: macroID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return linkRow{}, errLinkNotFound
	}
	if err != nil {
		return linkRow{}, err
	}
	return linkFromOwnedForThread(row), nil
}

// getMessage handles GET /email/messages/{id}.
func (d *Deps) getMessage(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid message id")
		return
	}
	ctx := r.Context()
	if _, err := d.ownedLinkForMessage(ctx, id, c.UserID); err != nil {
		if errors.Is(err, errLinkNotFound) {
			httpx.ErrorJSON(w, http.StatusNotFound, "message not found")
		} else {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch message")
		}
		return
	}
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

// getMessagesBatch handles POST /email/messages/batch — body: ["<uuid>",...].
func (d *Deps) getMessagesBatch(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var ids []uuid.UUID
	if !httpx.DecodeJSON(w, r, &ids) {
		return
	}
	if len(ids) == 0 || len(ids) > batchMessageLimit {
		httpx.ErrorJSON(w, http.StatusBadRequest, "must include between 1 and 100 message IDs")
		return
	}
	ctx := r.Context()

	// Keep only messages the caller can access (own or delegated links).
	var pgIDs []pgtype.UUID
	for _, id := range ids {
		pgIDs = append(pgIDs, pgUUID(id))
	}
	rows, err := d.q().GetParsedMessagesByIdBatch(ctx, pgIDs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch messages")
		return
	}
	// link accessibility check
	links, err := d.accessibleLinks(r, c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch links")
		return
	}
	linkSet := map[uuid.UUID]bool{}
	for _, l := range links {
		linkSet[l.ID] = true
	}
	var msgs []parsedMsg
	for _, row := range rows {
		pm := parsedFromBatchRow(row)
		if linkSet[pm.LinkID] {
			msgs = append(msgs, pm)
		}
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

// updateMessageLabels handles PATCH /email/messages/labels.
func (d *Deps) updateMessageLabels(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req UpdateLabelBatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if len(req.MessageIDs) == 0 || len(req.MessageIDs) > batchUpdateMessageLimit {
		httpx.ErrorJSON(w, http.StatusBadRequest, "Must include between 1 and 10 message IDs in request")
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	// label must belong to the link
	lbl, err := d.q().FetchLabelById(ctx, emaildb.FetchLabelByIdParams{
		ID: pgUUID(req.LabelID), LinkID: pgUUID(link.ID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "label not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch label")
		return
	}

	// Only messages the link owns can be mutated.
	var owned []pgtype.UUID
	var missing []uuid.UUID
	for _, mid := range req.MessageIDs {
		l, err := d.ownedLinkForMessage(ctx, mid, c.UserID)
		if err != nil || l.ID != link.ID {
			missing = append(missing, mid)
			continue
		}
		owned = append(owned, pgUUID(mid))
	}
	resp := UpdateLabelBatchResponse{
		SuccessfulIDs: []uuid.UUID{},
		FailedIDs:     []uuid.UUID{},
		MissingIDs:    missing,
	}
	if len(owned) > 0 {
		var opErr error
		if req.Value {
			opErr = d.q().InsertMessageLabelsBatch(ctx, emaildb.InsertMessageLabelsBatchParams{
				Column1: owned, LinkID: pgUUID(link.ID), ProviderLabelID: lbl.ProviderLabelID,
			})
		} else {
			opErr = d.q().DeleteMessageLabelsBatch(ctx, emaildb.DeleteMessageLabelsBatchParams{
				Column1: owned, LinkID: pgUUID(link.ID), ProviderLabelID: lbl.ProviderLabelID,
			})
		}
		if opErr != nil {
			for _, id := range owned {
				resp.FailedIDs = append(resp.FailedIDs, uuidFromPg(id))
			}
		} else {
			for _, id := range owned {
				resp.SuccessfulIDs = append(resp.SuccessfulIDs, uuidFromPg(id))
			}
			// best-effort provider sync
			d.enqueueLabelOps(ctx, link, owned, lbl.ProviderLabelID, req.Value)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// enqueueLabelOps publishes gmail_ops modify-labels jobs per message.
func (d *Deps) enqueueLabelOps(ctx context.Context, link linkRow, msgIDs []pgtype.UUID, providerLabelID string, add bool) {
	rows, err := d.q().GetSimpleMessagesBatch(ctx, emaildb.GetSimpleMessagesBatchParams{
		Column1: msgIDs, FusionauthUserID: link.FusionauthUserID,
	})
	if err != nil {
		slog.Warn("email: label op provider-id lookup failed", "err", err)
		return
	}
	for _, row := range rows {
		if !row.ProviderID.Valid {
			continue
		}
		data := map[string]any{"provider_message_id": row.ProviderID.String}
		if add {
			data["add_labels"] = []string{providerLabelID}
		} else {
			data["remove_labels"] = []string{providerLabelID}
		}
		d.publishJobBestEffort(ctx, subjGmailOps, "gmail_ops.modify_labels",
			gmailOps(link.ID, "modify_labels", data))
	}
}
