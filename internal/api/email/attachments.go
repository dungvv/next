package email

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// tempAttachmentKey mirrors generate_temp_attachment_s3_key! in Rust.
func tempAttachmentKey(linkID, attachmentID uuid.UUID, filename *string) string {
	name := ""
	if filename != nil {
		name = *filename
	}
	return fmt.Sprintf("temp/%s/%s-%s", linkID, attachmentID, name)
}

// getAttachment handles GET /email/attachments/{id}: returns the attachment
// metadata with a presigned data_url, fetching bytes from the provider and
// caching them in the attachment bucket when needed.
func (d *Deps) getAttachment(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	attID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid attachment id")
		return
	}
	ctx := r.Context()
	// resolve owning link via the attachment's thread
	trow, err := d.q().GetThreadIdForAttachment(ctx, pgUUID(attID))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "attachment does not exist")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch attachment")
		return
	}
	// caller must have access to the link owning the attachment
	linkOK, err := d.linkAccessible(ctx, uuidFromPg(trow.LinkID), c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to verify access")
		return
	}
	if !linkOK {
		httpx.ErrorJSON(w, http.StatusNotFound, "attachment does not exist")
		return
	}
	linkID := uuidFromPg(trow.LinkID)
	row, err := d.q().FetchAttachmentById(ctx, emaildb.FetchAttachmentByIdParams{
		ID: pgUUID(attID), LinkID: trow.LinkID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "attachment does not exist")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch attachment")
		return
	}
	att := MessageAttachment{
		DBID: uuidFromPg(row.ID), ProviderID: strPtrFromPg(row.ProviderAttachmentID),
		Filename: strPtrFromPg(row.Filename), MimeType: strPtrFromPg(row.MimeType),
		SizeBytes: i64PtrFromPg(row.SizeBytes), SfsID: uuidPtrFromPg(row.SfsID),
		ContentID: strPtrFromPg(row.ContentID),
	}
	if d.Store == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "object store unavailable")
		return
	}
	bucket := d.Cfg.AttachmentBucket
	key := tempAttachmentKey(linkID, attID, att.Filename)
	ttl := time.Duration(d.presignGetSecs()) * time.Second

	exists, _ := d.Store.Exists(ctx, bucket, key)
	if !exists {
		// fetch from provider and cache in object storage
		gc := d.gmailClient(ctx, linkID)
		if gc == nil {
			httpx.ErrorJSON(w, http.StatusServiceUnavailable, "email provider not configured")
			return
		}
		if att.ProviderID == nil || !row.MessageProviderID.Valid {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "attachment missing provider id")
			return
		}
		data, err := gc.GetAttachment(ctx, row.MessageProviderID.String, *att.ProviderID)
		if err != nil {
			httpx.ErrorJSON(w, http.StatusBadGateway, "error fetching attachment")
			return
		}
		mime := "application/octet-stream"
		if att.MimeType != nil {
			mime = *att.MimeType
		}
		if err := d.Store.PutObject(ctx, bucket, key, bytes.NewReader(data), mime); err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "error storing attachment")
			return
		}
	}
	url, err := d.Store.PresignGet(ctx, bucket, key, ttl)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "error fetching attachment")
		return
	}
	att.DataURL = &url
	httpx.WriteJSON(w, http.StatusOK, GetAttachmentResponse{Attachment: att})
}

// getAttachmentDocumentID handles GET /email/attachments/{id}/document_id —
// returns the macro document id this attachment was saved to (if any).
func (d *Deps) getAttachmentDocumentID(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	attID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "invalid attachment id")
		return
	}
	ctx := r.Context()
	trow, err := d.q().GetThreadIdForAttachment(ctx, pgUUID(attID))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.ErrorJSON(w, http.StatusNotFound, "attachment does not exist")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch attachment")
		return
	}
	linkOK, err := d.linkAccessible(ctx, uuidFromPg(trow.LinkID), c.UserID)
	if err != nil || !linkOK {
		httpx.ErrorJSON(w, http.StatusNotFound, "attachment does not exist")
		return
	}
	docID, err := d.q().GetDocumentIdByAttIdAndLink(ctx, emaildb.GetDocumentIdByAttIdAndLinkParams{
		LinkID: trow.LinkID, EmailAttachmentID: pgUUID(attID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteJSON(w, http.StatusOK, GetDocumentIDResponse{DocumentID: nil})
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch document id")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, GetDocumentIDResponse{DocumentID: &docID})
}

// linkAccessible reports whether macroID owns linkID directly or has it
// delegated through macro_user_links (fetch_inboxes_for_macro_id unions both).
func (d *Deps) linkAccessible(ctx context.Context, linkID uuid.UUID, macroID string) (bool, error) {
	rows, err := d.q().FetchInboxesForMacroId(ctx, macroID)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if uuidFromPg(row.ID) == linkID {
			return true, nil
		}
	}
	return false, nil
}
