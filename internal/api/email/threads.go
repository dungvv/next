package email

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

const (
	defaultMessageLimit = 5
	maxMessageLimit     = 100
)

// ParsedMessage mirrors models_email::service::message::ParsedMessage.
type ParsedMessage struct {
	DBID           uuid.UUID     `json:"db_id"`
	LinkID         uuid.UUID     `json:"link_id"`
	ThreadDBID     uuid.UUID     `json:"thread_db_id"`
	Subject        *string       `json:"subject"`
	From           *ContactInfo  `json:"from"`
	To             []ContactInfo `json:"to"`
	Cc             []ContactInfo `json:"cc"`
	Bcc            []ContactInfo `json:"bcc"`
	Labels         []LabelInfo   `json:"labels"`
	BodyParsed     *string       `json:"body_parsed"`
	InternalDateTs *string       `json:"internal_date_ts"`
}

// LabelInfo mirrors service::label::LabelInfo.
type LabelInfo struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
}

// getThread handles GET /email/threads/{id}.
func (d *Deps) getThread(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	ctx := r.Context()
	link, err := d.ownedLinkForThread(ctx, threadID, c.UserID)
	if err != nil {
		if errors.Is(err, errLinkNotFound) {
			httpx.ErrorJSON(w, http.StatusNotFound, "thread not found")
		} else {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread")
		}
		return
	}
	tr, err := d.q().GetThreadByIdAndLinkId(ctx, emaildb.GetThreadByIdAndLinkIdParams{
		ID: pgUUID(threadID), LinkID: pgUUID(link.ID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "thread not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread")
		return
	}

	rows, err := d.q().GetPaginatedParsedMessagesByThreadId(ctx,
		emaildb.GetPaginatedParsedMessagesByThreadIdParams{
			ThreadID: pgUUID(threadID), Limit: maxMessageLimit, Offset: 0,
		})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread messages")
		return
	}
	msgs := make([]parsedMsg, 0, len(rows))
	for _, row := range rows {
		msgs = append(msgs, parsedFromPaginatedRow(row))
	}
	aux, err := d.fetchAux(ctx, msgs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread messages")
		return
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		a := aux[m.ID]
		out = append(out, apiMessage(m, a.from, a.to, a.cc, a.bcc, a.labels, a.atts, a.drafts, a.fwd, a.sendTime))
	}

	access := "view"
	if link.MacroID == c.UserID {
		access = "owner"
	}
	httpx.WriteJSON(w, http.StatusOK, GetThreadResponse{Thread: Thread{
		DBID: threadID, ProviderID: strPtrFromPg(tr.ProviderID),
		LinkID: uuidFromPg(tr.LinkID), InboxVisible: tr.InboxVisible,
		IsRead: tr.IsRead, AccessLevel: access,
		LatestInboundMessageTs:  tsPtrFromPg(tr.LatestInboundMessageTs),
		LatestOutboundMessageTs: tsPtrFromPg(tr.LatestOutboundMessageTs),
		LatestNonSpamMessageTs:  tsPtrFromPg(tr.LatestNonSpamMessageTs),
		CreatedAt:               tsFromPg(tr.CreatedAt),
		UpdatedAt:               tsFromPg(tr.UpdatedAt),
		Messages:                out,
	}})
}

// getThreadMessages handles GET /email/threads/{id}/messages?offset&limit.
func (d *Deps) getThreadMessages(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	offset, limit, ok := parsePaging(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	links, err := d.accessibleLinks(r, c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch links")
		return
	}
	linkSet := map[uuid.UUID]bool{}
	for _, l := range links {
		linkSet[l.ID] = true
	}
	rows, err := d.q().GetPaginatedParsedMessagesByThreadId(ctx,
		emaildb.GetPaginatedParsedMessagesByThreadIdParams{
			ThreadID: pgUUID(threadID), Limit: int32(limit), Offset: int32(offset),
		})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch messages")
		return
	}
	var msgs []parsedMsg
	for _, row := range rows {
		pm := parsedFromPaginatedRow(row)
		if linkSet[pm.LinkID] {
			msgs = append(msgs, pm)
		}
	}
	if len(msgs) == 0 {
		httpx.ErrorJSON(w, http.StatusNotFound, "thread not found")
		return
	}
	aux, err := d.fetchAux(ctx, msgs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch message details")
		return
	}
	out := make([]ParsedMessage, 0, len(msgs))
	for _, m := range msgs {
		a := aux[m.ID]
		var bodyParsed *string
		if m.BodyHTMLSanitized != nil {
			bp := computeBodyParsed(*m.BodyHTMLSanitized)
			bodyParsed = &bp
		}
		var labels []LabelInfo
		for _, l := range a.labels {
			name := ""
			if l.Name != nil {
				name = *l.Name
			}
			labels = append(labels, LabelInfo{ProviderID: l.ProviderLabelID, Name: name})
		}
		var ts *string
		if m.InternalDateTs != nil {
			s := m.InternalDateTs.UTC().Format("2006-01-02T15:04:05.999999999Z")
			ts = &s
		}
		out = append(out, ParsedMessage{
			DBID: m.ID, LinkID: m.LinkID, ThreadDBID: m.ThreadID,
			Subject: m.Subject, From: a.from,
			To: a.to, Cc: a.cc, Bcc: a.bcc,
			Labels: labels, BodyParsed: bodyParsed, InternalDateTs: ts,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// threadSeen handles POST /email/threads/{id}/seen — mark all messages read.
func (d *Deps) threadSeen(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	ctx := r.Context()
	link, err := d.ownedLinkForThread(ctx, threadID, c.UserID)
	if err != nil {
		if errors.Is(err, errLinkNotFound) {
			httpx.ErrorJSON(w, http.StatusNotFound, "thread not found")
		} else {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread")
		}
		return
	}
	// Load messages to know which unread ones need provider updates.
	rows, err := d.q().FetchMessagesWithLabels(ctx, emaildb.FetchMessagesWithLabelsParams{
		ThreadID: pgUUID(threadID), LinkID: pgUUID(link.ID),
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch messages")
		return
	}
	var ids []pgtype.UUID
	for _, m := range rows {
		if !m.IsRead {
			ids = append(ids, m.ID)
		}
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(ctx)
	qtx := d.q().WithTx(tx)
	if len(ids) > 0 {
		if _, err := qtx.UpdateMessageReadStatusBatch(ctx, emaildb.UpdateMessageReadStatusBatchParams{
			IsRead: true, Column2: ids, LinkID: pgUUID(link.ID),
		}); err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to update messages")
			return
		}
	}
	if err := qtx.UpdateThreadReadStatus(ctx, emaildb.UpdateThreadReadStatusParams{
		IsRead: true, ID: pgUUID(threadID), LinkID: pgUUID(link.ID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to update thread")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "db error")
		return
	}
	// Best-effort: strip UNREAD at the provider.
	for _, m := range rows {
		if m.IsRead || !m.ProviderID.Valid {
			continue
		}
		d.publishJobBestEffort(ctx, subjGmailOps, "gmail_ops.modify_labels",
			gmailOps(link.ID, "modify_labels", map[string]any{
				"provider_message_id": m.ProviderID.String,
				"remove_labels":       []string{"UNREAD"},
			}))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// threadArchived handles PATCH /email/threads/{id}/archived {value}.
func (d *Deps) threadArchived(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	var req ArchiveThreadRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	link, err := d.ownedLinkForThread(ctx, threadID, c.UserID)
	if err != nil {
		if errors.Is(err, errLinkNotFound) {
			httpx.ErrorJSON(w, http.StatusNotFound, "thread not found")
		} else {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread")
		}
		return
	}
	thread, err := d.q().GetThreadByIdAndLinkId(ctx, emaildb.GetThreadByIdAndLinkIdParams{
		ID: pgUUID(threadID), LinkID: pgUUID(link.ID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "thread not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread")
		return
	}
	// value=true means archive → inbox_visible=false
	wantVisible := !req.Value
	if thread.InboxVisible == wantVisible {
		httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
		return
	}
	if err := d.q().UpdateInboxVisibleStatus(ctx, emaildb.UpdateInboxVisibleStatusParams{
		InboxVisible: wantVisible, ID: pgUUID(threadID), LinkID: pgUUID(link.ID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to update thread")
		return
	}
	// Best-effort provider sync: add/remove INBOX label on each message.
	rows, err := d.q().FetchMessagesWithLabels(ctx, emaildb.FetchMessagesWithLabelsParams{
		ThreadID: pgUUID(threadID), LinkID: pgUUID(link.ID),
	})
	if err == nil {
		for _, m := range rows {
			if !m.ProviderID.Valid {
				continue
			}
			data := map[string]any{"provider_message_id": m.ProviderID.String}
			if req.Value {
				data["remove_labels"] = []string{"INBOX"}
			} else {
				data["add_labels"] = []string{"INBOX"}
			}
			d.publishJobBestEffort(ctx, subjGmailOps, "gmail_ops.modify_labels",
				gmailOps(link.ID, "modify_labels", data))
		}
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// updateThreadLabels handles PATCH /email/threads/{id}/labels {label_id, value}.
func (d *Deps) updateThreadLabels(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	var req UpdateThreadLabelRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	link, err := d.ownedLinkForThread(ctx, threadID, c.UserID)
	if err != nil {
		if errors.Is(err, errLinkNotFound) {
			httpx.ErrorJSON(w, http.StatusNotFound, "thread not found")
		} else {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch thread")
		}
		return
	}
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
	rows, err := d.q().FetchMessagesWithLabels(ctx, emaildb.FetchMessagesWithLabelsParams{
		ThreadID: pgUUID(threadID), LinkID: pgUUID(link.ID),
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch messages")
		return
	}
	var ids []pgtype.UUID
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	resp := UpdateThreadLabelsResponse{SuccessfulIDs: []uuid.UUID{}, FailedIDs: []uuid.UUID{}}
	if len(ids) == 0 {
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}
	var opErr error
	if req.Value {
		opErr = d.q().InsertMessageLabelsBatch(ctx, emaildb.InsertMessageLabelsBatchParams{
			Column1: ids, LinkID: pgUUID(link.ID), ProviderLabelID: lbl.ProviderLabelID,
		})
	} else {
		opErr = d.q().DeleteMessageLabelsBatch(ctx, emaildb.DeleteMessageLabelsBatchParams{
			Column1: ids, LinkID: pgUUID(link.ID), ProviderLabelID: lbl.ProviderLabelID,
		})
	}
	if opErr != nil {
		for _, id := range ids {
			resp.FailedIDs = append(resp.FailedIDs, uuidFromPg(id))
		}
	} else {
		for _, id := range ids {
			resp.SuccessfulIDs = append(resp.SuccessfulIDs, uuidFromPg(id))
		}
		d.enqueueLabelOps(ctx, link, ids, lbl.ProviderLabelID, req.Value)
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func parsePaging(w http.ResponseWriter, r *http.Request) (offset, limit int64, ok bool) {
	offset, limit = 0, defaultMessageLimit
	if s := r.URL.Query().Get("offset"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			httpx.ErrorJSON(w, http.StatusBadRequest, "offset must be non-negative")
			return 0, 0, false
		}
		offset = v
	}
	if s := r.URL.Query().Get("limit"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 {
			httpx.ErrorJSON(w, http.StatusBadRequest, "limit must be positive")
			return 0, 0, false
		}
		if v > maxMessageLimit {
			httpx.ErrorJSON(w, http.StatusBadRequest, "limit must not exceed 100")
			return 0, 0, false
		}
		limit = v
	}
	return offset, limit, true
}
