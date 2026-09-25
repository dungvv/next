package email

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/microcosm-cc/bluemonday"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

var htmlSanitizer = bluemonday.UGCPolicy()

// draftAttachmentsKey mirrors generate_attachment_s3_key! in the Rust service.
func draftAttachmentS3Key(draftID, attachmentID uuid.UUID) string {
	return fmt.Sprintf("draft/%s/%s", draftID, attachmentID)
}

// decodeBodyHTML decodes the base64 URL_SAFE_NO_PAD encoded HTML body the API
// expects, then sanitizes it for storage in body_html_sanitized.
func decodeBodyHTML(b64 *string) (*string, error) {
	if b64 == nil || *b64 == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(*b64)
	if err != nil {
		// fall back to treating it as literal HTML
		s := htmlSanitizer.Sanitize(*b64)
		return &s, nil
	}
	s := htmlSanitizer.Sanitize(string(raw))
	return &s, nil
}

// upsertDraftSQL mirrors the Rust upsert_draft guarded insert/update: an
// existing row is only rewritten when it is an unsent draft in this inbox.
const upsertDraftSQL = `
INSERT INTO email_messages (
    id, provider_id, link_id, thread_id, provider_thread_id,
    replying_to_id, subject, from_contact_id, sent_at,
    has_attachments, is_read, is_starred, is_sent, is_draft,
    body_text, body_html_sanitized, body_macro, headers_jsonb,
    created_at, updated_at
)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
ON CONFLICT (id) DO UPDATE SET
    provider_id = EXCLUDED.provider_id,
    thread_id = EXCLUDED.thread_id,
    provider_thread_id = EXCLUDED.provider_thread_id,
    replying_to_id = EXCLUDED.replying_to_id,
    subject = EXCLUDED.subject,
    from_contact_id = EXCLUDED.from_contact_id,
    sent_at = EXCLUDED.sent_at,
    is_read = EXCLUDED.is_read,
    is_starred = EXCLUDED.is_starred,
    is_sent = EXCLUDED.is_sent,
    is_draft = EXCLUDED.is_draft,
    body_text = EXCLUDED.body_text,
    body_html_sanitized = EXCLUDED.body_html_sanitized,
    body_macro = EXCLUDED.body_macro,
    headers_jsonb = EXCLUDED.headers_jsonb,
    updated_at = NOW()
WHERE email_messages.link_id = EXCLUDED.link_id
    AND email_messages.is_draft
    AND NOT email_messages.is_sent
RETURNING id`

var errDraftGuardRejected = errors.New("draft row missing or no longer a draft")

// upsertDraftTx inserts or updates the draft message row inside tx.
func upsertDraftTx(ctx context.Context, tx pgx.Tx, in DraftInput, link linkRow, msgID, threadID uuid.UUID, fromContactID *uuid.UUID) (uuid.UUID, error) {
	html, err := decodeBodyHTML(in.BodyHTML)
	if err != nil {
		return uuid.Nil, err
	}
	now := time.Now().UTC()
	var headers []byte
	if len(in.HeadersJSON) > 0 && string(in.HeadersJSON) != "null" {
		headers = in.HeadersJSON
	}
	var id pgtype.UUID
	err = tx.QueryRow(ctx, upsertDraftSQL,
		pgUUID(msgID), pgText(in.ProviderID), pgUUID(link.ID), pgUUID(threadID),
		pgText(in.ProviderThreadID), pgUUIDPtr(in.ReplyingToID), pgTextStr(in.Subject),
		pgUUIDPtr(fromContactID), pgtype.Timestamptz{Time: now, Valid: true},
		false, true, false, false, true,
		pgText(in.BodyText), pgText(html), pgText(in.BodyMacro), headers,
		pgtype.Timestamptz{Time: now, Valid: true}, pgtype.Timestamptz{Time: now, Valid: true},
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errDraftGuardRejected
	}
	if err != nil {
		return uuid.Nil, err
	}
	return uuidFromPg(id), nil
}

// upsertRecipientsTx replaces the draft's recipients inside tx.
func upsertRecipientsTx(ctx context.Context, qtx *emaildb.Queries, msgID uuid.UUID, link linkRow, in DraftInput) error {
	type rc struct {
		email string
		name  string
		typ   emaildb.EmailRecipientType
	}
	var all []rc
	push := func(list []DraftContactInfo, typ emaildb.EmailRecipientType) {
		for _, c := range list {
			if c.Email == "" {
				continue
			}
			n := ""
			if c.Name != nil {
				n = *c.Name
			}
			all = append(all, rc{email: c.Email, name: n, typ: typ})
		}
	}
	push(in.To, emaildb.EmailRecipientTypeTO)
	push(in.Cc, emaildb.EmailRecipientTypeCC)
	push(in.Bcc, emaildb.EmailRecipientTypeBCC)

	// resolve/create contacts
	byEmail := map[string]pgtype.UUID{}
	if len(all) > 0 {
		emails := make([]string, 0, len(all))
		seen := map[string]bool{}
		for _, c := range all {
			if !seen[c.email] {
				seen[c.email] = true
				emails = append(emails, c.email)
			}
		}
		existing, err := qtx.FetchContactsByEmails(ctx, emaildb.FetchContactsByEmailsParams{
			LinkID: pgUUID(link.ID), Column2: emails,
		})
		if err != nil {
			return err
		}
		for _, e := range existing {
			byEmail[e.EmailAddress] = e.ID
		}
		var ids, lids []pgtype.UUID
		var ems, names []string
		for _, c := range all {
			if _, ok := byEmail[c.email]; ok {
				continue
			}
			nid := pgUUID(uuid.New())
			byEmail[c.email] = nid
			ids = append(ids, nid)
			lids = append(lids, pgUUID(link.ID))
			ems = append(ems, c.email)
			names = append(names, c.name)
		}
		if len(ids) > 0 {
			if _, err := qtx.InsertNewContacts(ctx, emaildb.InsertNewContactsParams{
				Column1: ids, Column2: lids, Column3: ems, Column4: names,
			}); err != nil {
				return err
			}
		}
	}
	// prune + insert recipients
	var contactIDs []pgtype.UUID
	var types []emaildb.EmailRecipientType
	var msgIDs []pgtype.UUID
	var names []string
	seenPair := map[string]bool{}
	for _, c := range all {
		cid, ok := byEmail[c.email]
		if !ok {
			continue
		}
		key := string(cid.Bytes[:]) + string(c.typ)
		if seenPair[key] {
			continue
		}
		seenPair[key] = true
		contactIDs = append(contactIDs, cid)
		types = append(types, c.typ)
		msgIDs = append(msgIDs, pgUUID(msgID))
		names = append(names, c.name)
	}
	if err := qtx.UpsertMessageRecipients(ctx, emaildb.UpsertMessageRecipientsParams{
		MessageID: pgUUID(msgID), Column2: contactIDs, Column3: types,
	}); err != nil {
		return err
	}
	if len(contactIDs) > 0 {
		if err := qtx.UpsertMessageRecipients2(ctx, emaildb.UpsertMessageRecipients2Params{
			Column1: msgIDs, Column2: contactIDs, Column3: names, Column4: types,
		}); err != nil {
			return err
		}
	}
	return nil
}

// draftContact converts a recipient row back to the API shape.
func draftContact(c ContactInfo) DraftContactInfo {
	return DraftContactInfo{Email: c.Email, Name: c.Name, PhotoURL: c.PhotoURL}
}

// createDraft handles POST /email/drafts — creates or updates a draft.
func (d *Deps) createDraft(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req CreateDraftRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	out, err := d.saveDraft(r.Context(), link, req.Draft, req.SendTime)
	if err != nil {
		switch {
		case errors.Is(err, errDraftGuardRejected), errors.Is(err, errLinkNotFound):
			httpx.ErrorJSON(w, http.StatusNotFound, "draft not found")
		default:
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to save draft")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, CreateDraftResponse{Draft: *out})
}

// saveDraft performs the draft upsert inside a transaction and returns the
// API draft output.
func (d *Deps) saveDraft(ctx context.Context, link linkRow, in DraftInput, sendTime *time.Time) (*DraftOutput, error) {
	// Resolve thread: use provided thread_db_id (validated), else create one.
	threadID := in.ThreadDBID
	if threadID != nil {
		if _, err := d.q().GetThreadByIdAndLinkId(ctx, emaildb.GetThreadByIdAndLinkIdParams{
			ID: pgUUID(*threadID), LinkID: pgUUID(link.ID),
		}); err != nil {
			return nil, errLinkNotFound
		}
	}
	if threadID == nil && in.ReplyingToID != nil {
		// replying → reuse the replied-to message's thread
		src, err := d.q().GetSimpleMessage(ctx, emaildb.GetSimpleMessageParams{
			ID: pgUUID(*in.ReplyingToID), FusionauthUserID: link.FusionauthUserID,
		})
		if err == nil && src.ThreadID.Valid {
			tid := uuidFromPg(src.ThreadID)
			threadID = &tid
		}
	}

	msgID := uuid.New()
	if in.DBID != nil {
		msgID = *in.DBID
		// Must already be a draft owned by this link.
		exists, err := d.q().DraftExistsWithId(ctx, emaildb.DraftExistsWithIdParams{
			ID: pgUUID(msgID), LinkID: pgUUID(link.ID),
		})
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, errDraftGuardRejected
		}
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	qtx := d.q().WithTx(tx)

	if threadID == nil {
		tid, err := qtx.InsertThread(ctx, emaildb.InsertThreadParams{
			ID:         pgUUID(uuid.New()),
			ProviderID: pgText(in.ProviderThreadID),
			LinkID:     pgUUID(link.ID),
		})
		if err != nil {
			return nil, err
		}
		t := uuidFromPg(tid)
		threadID = &t
	}

	// resolve the sender contact (the inbox's own address)
	var fromID *uuid.UUID
	if row, err := qtx.FetchContactByEmail(ctx, emaildb.FetchContactByEmailParams{
		Lower: link.EmailAddress, LinkID: pgUUID(link.ID),
	}); err == nil {
		id := uuidFromPg(row.ID)
		fromID = &id
	}

	if _, err := upsertDraftTx(ctx, tx, in, link, msgID, *threadID, fromID); err != nil {
		return nil, err
	}
	if err := upsertRecipientsTx(ctx, qtx, msgID, link, in); err != nil {
		return nil, err
	}
	if sendTime != nil {
		if err := qtx.UpsertScheduledMessage(ctx, emaildb.UpsertScheduledMessageParams{
			LinkID: pgUUID(link.ID), MessageID: pgUUID(msgID),
			SendTime: pgTs(sendTime), Sent: false,
			ActorID: pgTextStr(link.MacroID),
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &DraftOutput{
		DBID: &msgID, ProviderID: in.ProviderID, ReplyingToID: in.ReplyingToID,
		ProviderThreadID: in.ProviderThreadID, ThreadDBID: threadID,
		LinkID: link.ID, Subject: in.Subject,
		To: draftContacts(in.To), Cc: draftContacts(in.Cc), Bcc: draftContacts(in.Bcc),
		BodyText: in.BodyText, BodyHTML: decodedBodyForOutput(in.BodyHTML),
		BodyMacro: in.BodyMacro, HeadersJSON: in.HeadersJSON, SendTime: sendTime,
	}, nil
}

func draftContacts(in []DraftContactInfo) []DraftContactInfo {
	if len(in) == 0 {
		return nil
	}
	return in
}

func decodedBodyForOutput(b64 *string) *string {
	s, _ := decodeBodyHTML(b64)
	return s
}

// deleteDraft handles DELETE /email/drafts/{id} — deletes the draft row, its
// recipients/labels, scheduled row, and S3 draft attachments.
func (d *Deps) deleteDraft(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid draft id")
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	exists, err := d.q().DraftExistsWithId(ctx, emaildb.DraftExistsWithIdParams{
		ID: pgUUID(id), LinkID: pgUUID(link.ID),
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check draft")
		return
	}
	if !exists {
		httpx.ErrorJSON(w, http.StatusNotFound, "draft not found")
		return
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(ctx)
	qtx := d.q().WithTx(tx)
	// collect draft attachment s3 keys before deleting rows
	var s3keys []string
	if atts, err := qtx.FetchDraftAttachmentsByDraftId(ctx, emaildb.FetchDraftAttachmentsByDraftIdParams{
		DraftID: pgUUID(id), LinkID: pgUUID(link.ID),
	}); err == nil {
		for _, a := range atts {
			s3keys = append(s3keys, a.S3Key)
		}
	}
	for _, fn := range []func(context.Context) error{
		func(ctx context.Context) error {
			return qtx.DeleteScheduledMessage(ctx, emaildb.DeleteScheduledMessageParams{
				LinkID: pgUUID(link.ID), MessageID: pgUUID(id),
			})
		},
		func(ctx context.Context) error { return qtx.DeleteMessageRecipients(ctx, pgUUID(id)) },
		func(ctx context.Context) error { return qtx.DeleteAllMessageLabels(ctx, pgUUID(id)) },
		func(ctx context.Context) error { return qtx.DeleteMessageAttachments(ctx, pgUUID(id)) },
		func(ctx context.Context) error { return qtx.DeleteDbMessage(ctx, pgUUID(id)) },
	} {
		if err := fn(ctx); err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to delete draft")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "db error")
		return
	}
	if len(s3keys) > 0 && d.Store != nil {
		d.Store.DeleteObjects(ctx, d.Cfg.AttachmentBucket, s3keys)
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// addDraftAttachment handles POST /email/drafts/{id}/attachments — registers
// an upload and returns a presigned PUT URL into the attachments bucket.
func (d *Deps) addDraftAttachment(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	draftID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid draft id")
		return
	}
	var req AddDraftAttachmentRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.FileName == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "file_name is required")
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	exists, err := d.q().DraftExistsWithId(ctx, emaildb.DraftExistsWithIdParams{
		ID: pgUUID(draftID), LinkID: pgUUID(link.ID),
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check draft")
		return
	}
	if !exists {
		httpx.ErrorJSON(w, http.StatusNotFound, "draft not found")
		return
	}
	mime := mimeTypeForFilename(req.FileName)
	attID := uuid.New()
	s3key := draftAttachmentS3Key(draftID, attID)
	if err := d.q().InsertDraftAttachment(ctx, emaildb.InsertDraftAttachmentParams{
		ID: pgUUID(attID), DraftID: pgUUID(draftID), FileName: req.FileName,
		ContentType: mime, Sha: req.Sha, Size: req.Size, S3Key: s3key,
		LinkID: pgUUID(link.ID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create attachment")
		return
	}
	if d.Store == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "object store unavailable")
		return
	}
	url, err := d.Store.PresignPut(ctx, d.Cfg.AttachmentBucket, s3key, mime,
		time.Duration(d.presignGetSecs())*time.Second)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create upload url")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, AddDraftAttachmentResponse{
		AttachmentID: attID, UploadURL: url, ContentType: mime,
	})
}

// removeDraftAttachment handles DELETE /email/drafts/{id}/attachments/{aid}.
func (d *Deps) removeDraftAttachment(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	draftID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid draft id")
		return
	}
	attID, err := uuid.Parse(chi.URLParam(r, "attachment_id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid attachment id")
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	s3key := draftAttachmentS3Key(draftID, attID)
	if err := d.q().DeleteDraftAttachment(ctx, emaildb.DeleteDraftAttachmentParams{
		ID: pgUUID(attID), DraftID: pgUUID(draftID), LinkID: pgUUID(link.ID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to delete attachment")
		return
	}
	if d.Store != nil {
		_ = d.Store.DeleteObject(ctx, d.Cfg.AttachmentBucket, s3key)
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// addForwardedAttachment handles POST /email/drafts/{id}/forwarded-attachments
// — attaches a copy-reference of an existing provider attachment to a draft.
func (d *Deps) addForwardedAttachment(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	draftID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid draft id")
		return
	}
	var req AddForwardedAttachmentRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	exists, err := d.q().DraftExistsWithId(ctx, emaildb.DraftExistsWithIdParams{
		ID: pgUUID(draftID), LinkID: pgUUID(link.ID),
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check draft")
		return
	}
	if !exists {
		httpx.ErrorJSON(w, http.StatusNotFound, "draft not found")
		return
	}
	if err := d.q().InsertForwardedAttachment(ctx, emaildb.InsertForwardedAttachmentParams{
		MessageID: pgUUID(draftID), AttachmentID: pgUUID(req.AttachmentID),
		LinkID: pgUUID(link.ID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to attach forwarded attachment")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// removeForwardedAttachment handles
// DELETE /email/drafts/{id}/forwarded-attachments/{aid}.
func (d *Deps) removeForwardedAttachment(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	draftID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid draft id")
		return
	}
	attID, err := uuid.Parse(chi.URLParam(r, "attachment_id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid attachment id")
		return
	}
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	if err := d.q().DeleteForwardedAttachment(r.Context(), emaildb.DeleteForwardedAttachmentParams{
		MessageID: pgUUID(draftID), AttachmentID: pgUUID(attID), LinkID: pgUUID(link.ID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to remove attachment")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// listScheduledDrafts handles GET /email/drafts/scheduled?offset&limit.
func (d *Deps) listScheduledDrafts(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	offset, limit, ok := parsePaging(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	rows, err := d.q().GetScheduledDbMessagesByLinkId(ctx,
		emaildb.GetScheduledDbMessagesByLinkIdParams{
			LinkID: pgUUID(link.ID), Limit: int32(limit), Offset: int32(offset),
		})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch scheduled drafts")
		return
	}
	msgs := make([]parsedMsg, 0, len(rows))
	for _, row := range rows {
		msgs = append(msgs, parsedFromScheduledRow(row))
	}
	aux, err := d.fetchAux(ctx, msgs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch scheduled drafts")
		return
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		a := aux[m.ID]
		out = append(out, apiMessage(m, a.from, a.to, a.cc, a.bcc, a.labels, a.atts, a.drafts, a.fwd, a.sendTime))
	}
	httpx.WriteJSON(w, http.StatusOK, GetScheduledResponse{Messages: out})
}

// upsertScheduledDraft handles PUT /email/drafts/scheduled/{message_id}.
func (d *Deps) upsertScheduledDraft(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	msgID, err := uuid.Parse(chi.URLParam(r, "message_id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid message id")
		return
	}
	var req UpsertScheduledRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	exists, err := d.q().DraftExistsWithId(ctx, emaildb.DraftExistsWithIdParams{
		ID: pgUUID(msgID), LinkID: pgUUID(link.ID),
	})
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check draft")
		return
	}
	if !exists {
		httpx.ErrorJSON(w, http.StatusNotFound, "draft not found")
		return
	}
	if err := d.q().UpsertScheduledMessage(ctx, emaildb.UpsertScheduledMessageParams{
		LinkID: pgUUID(link.ID), MessageID: pgUUID(msgID),
		SendTime: pgTs(&req.SendTime), Sent: false,
		ActorID: pgTextStr(link.MacroID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to schedule draft")
		return
	}
	d.publishJobBestEffort(ctx, subjEmailSend, "email.send_scheduled", SendJobMsg{
		LinkID: link.ID, MessageID: msgID, ActorID: link.MacroID,
	})
	httpx.WriteJSON(w, http.StatusOK, UpsertScheduledResponse{
		MessageID: msgID, SendTime: req.SendTime,
	})
}

// deleteScheduledDraft handles DELETE /email/drafts/scheduled/{message_id}.
func (d *Deps) deleteScheduledDraft(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	msgID, err := uuid.Parse(chi.URLParam(r, "message_id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid message id")
		return
	}
	ctx := r.Context()
	link, ok := d.resolveLink(w, r, c)
	if !ok {
		return
	}
	sched, err := d.q().GetScheduledMessage(ctx, emaildb.GetScheduledMessageParams{
		LinkID: pgUUID(link.ID), MessageID: pgUUID(msgID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "scheduled message not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch scheduled message")
		return
	}
	if sched.Sent {
		httpx.ErrorJSON(w, http.StatusBadRequest, "message already sent")
		return
	}
	if err := d.q().DeleteScheduledMessage(ctx, emaildb.DeleteScheduledMessageParams{
		LinkID: pgUUID(link.ID), MessageID: pgUUID(msgID),
	}); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to delete scheduled message")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Deps) presignGetSecs() int {
	if d.Cfg.PresignGetSecs > 0 {
		return d.Cfg.PresignGetSecs
	}
	return 3600
}

var mimeByExt = map[string]string{
	"pdf": "application/pdf", "png": "image/png", "jpg": "image/jpeg",
	"jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp",
	"txt": "text/plain", "csv": "text/csv", "html": "text/html",
	"doc":  "application/msword",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls":  "application/vnd.ms-excel",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"zip":  "application/zip", "ics": "text/calendar",
	"eml": "message/rfc822", "json": "application/json",
}

// mimeTypeForFilename maps a filename extension to a MIME type (mirrors the
// Rust FileType → ContentType mapping for common cases).
func mimeTypeForFilename(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i < len(name)-1 {
		if m, ok := mimeByExt[strings.ToLower(name[i+1:])]; ok {
			return m
		}
	}
	return "application/octet-stream"
}
