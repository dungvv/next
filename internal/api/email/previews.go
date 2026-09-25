package email

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
)

const (
	defaultPreviewLimit = 25
	maxPreviewLimit     = 100
)

// viewPredicate returns the WHERE clause fragment for a preview view.
// Mirrors the Rust PreviewView semantics closely enough for the common views.
func viewPredicate(view string) (string, bool) {
	switch view {
	case "inbox":
		// inbox-visible threads that aren't entirely in TRASH/SPAM.
		return `t.inbox_visible = TRUE AND NOT EXISTS (
				SELECT 1 FROM email_messages m
				JOIN email_message_labels ml ON ml.message_id = m.id
				JOIN email_labels lb ON lb.id = ml.label_id
				WHERE m.thread_id = t.id AND lb.link_id = t.link_id
				  AND lb.provider_label_id IN ('TRASH','SPAM')
			)`, true
	case "sent":
		return `t.latest_outbound_message_ts IS NOT NULL`, true
	case "drafts":
		return `EXISTS (SELECT 1 FROM email_messages m WHERE m.thread_id = t.id AND m.is_draft)`, true
	case "starred":
		return `EXISTS (SELECT 1 FROM email_messages m WHERE m.thread_id = t.id AND m.is_starred)`, true
	case "all":
		return `TRUE`, true
	case "important":
		return `EXISTS (
				SELECT 1 FROM email_messages m
				JOIN email_message_labels ml ON ml.message_id = m.id
				JOIN email_labels lb ON lb.id = ml.label_id
				WHERE m.thread_id = t.id AND lb.link_id = t.link_id
				  AND lb.provider_label_id = 'IMPORTANT'
			)`, true
	case "other":
		return `t.inbox_visible = FALSE`, true
	}
	if strings.HasPrefix(view, "user:") && strings.TrimPrefix(view, "user:") != "" {
		return `EXISTS (
				SELECT 1 FROM email_messages m
				JOIN email_message_labels ml ON ml.message_id = m.id
				JOIN email_labels lb ON lb.id = ml.label_id
				WHERE m.thread_id = t.id AND lb.link_id = t.link_id
				  AND lb.provider_label_id = @pid::text
			)`, true
	}
	return "", false
}

// previewRow is one page row from the preview query.
type previewRow struct {
	ID             uuid.UUID
	ProviderID     *string
	LinkID         uuid.UUID
	OwnerID        string
	InboxVisible   bool
	IsRead         bool
	IsDraft        bool
	IsImportant    bool
	Name           *string
	Snippet        *string
	SenderEmail    *string
	SenderName     *string
	SenderPhotoURL *string
	SortTs         time.Time
	ViewedAt       *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// previewQuery pages threads for the caller's link set, filtered by view.
// Modeled on GetThreadSummaryInfo's latest/earliest message selection.
const previewQuery = `
SELECT
    t.id, t.provider_id, t.link_id, t.inbox_visible, t.is_read,
    l.macro_id AS owner_id,
    EXISTS (SELECT 1 FROM email_messages md WHERE md.thread_id = t.id AND md.is_draft) AS is_draft,
    EXISTS (
        SELECT 1 FROM email_messages mi
        JOIN email_message_labels ml ON ml.message_id = mi.id
        JOIN email_labels lb ON lb.id = ml.label_id
        WHERE mi.thread_id = t.id AND lb.link_id = t.link_id AND lb.provider_label_id = 'IMPORTANT'
    ) AS is_important,
    earliest_msg.subject AS name,
    latest_msg.snippet,
    latest_msg.sender AS sender_email,
    latest_msg.pretty_sender AS sender_name,
    latest_msg.photo_url AS sender_photo_url,
    COALESCE(t.latest_non_spam_message_ts, t.latest_inbound_message_ts,
             t.latest_outbound_message_ts, t.created_at) AS sort_ts,
    uh.updated_at AS viewed_at,
    t.created_at, t.updated_at
FROM email_threads t
JOIN email_links l ON l.id = t.link_id
LEFT JOIN email_user_history uh ON uh.thread_id = t.id AND uh.link_id = t.link_id
JOIN LATERAL (
    SELECT m2.snippet, c.email_address AS sender,
           COALESCE(m2.from_name, c.name, c.email_address) AS pretty_sender,
           c.sfs_photo_url AS photo_url
    FROM email_messages m2
    LEFT JOIN email_contacts c ON c.id = m2.from_contact_id
    WHERE m2.thread_id = t.id AND m2.link_id = t.link_id
    ORDER BY
        (CASE WHEN m2.is_draft = false AND m2.sent_at IS NOT NULL THEN 0 ELSE 1 END) ASC,
        COALESCE(m2.sent_at, m2.updated_at) DESC NULLS LAST
    LIMIT 1
) latest_msg ON true
LEFT JOIN LATERAL (
    SELECT m3.subject
    FROM email_messages m3
    WHERE m3.thread_id = t.id AND m3.link_id = t.link_id
    ORDER BY
        (CASE WHEN m3.is_draft = false AND m3.sent_at IS NOT NULL THEN 0 ELSE 1 END) ASC,
        COALESCE(m3.sent_at, m3.updated_at) ASC NULLS LAST
    LIMIT 1
) earliest_msg ON true
WHERE t.link_id = ANY($1::uuid[])
  AND %s
  AND ($3::timestamptz IS NULL OR
       (COALESCE(t.latest_non_spam_message_ts, t.latest_inbound_message_ts,
                 t.latest_outbound_message_ts, t.created_at), t.id) < ($3, $4::uuid))
ORDER BY sort_ts DESC, t.id DESC
LIMIT $2`

// getThreadPreviews handles GET /email/threads/previews/cursor/{view}?cursor&limit.
func (d *Deps) getThreadPreviews(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	view := chi.URLParam(r, "view")
	pred, ok := viewPredicate(view)
	if !ok {
		httpx.ErrorJSON(w, http.StatusBadRequest, "unsupported view")
		return
	}
	limit := defaultPreviewLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			httpx.ErrorJSON(w, http.StatusBadRequest, "limit must be positive")
			return
		}
		if v > maxPreviewLimit {
			v = maxPreviewLimit
		}
		limit = v
	}
	var curTs *time.Time
	var curID uuid.UUID
	if cur := r.URL.Query().Get("cursor"); cur != "" {
		raw, err := base64.StdEncoding.DecodeString(cur)
		if err != nil {
			httpx.ErrorJSON(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		parts := strings.SplitN(string(raw), "|", 2)
		if len(parts) != 2 {
			httpx.ErrorJSON(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		ts, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			httpx.ErrorJSON(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		id, err := uuid.Parse(parts[1])
		if err != nil {
			httpx.ErrorJSON(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		curTs = &ts
		curID = id
	}
	ctx := r.Context()
	links, err := d.accessibleLinks(r, c.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch links")
		return
	}
	linkIDs := make([]pgtype.UUID, 0, len(links))
	for _, l := range links {
		linkIDs = append(linkIDs, pgUUID(l.ID))
	}

	args := []any{linkIDs, limit + 1, pgTs(curTs), pgUUID(curID)}
	if strings.HasPrefix(view, "user:") {
		pred = strings.ReplaceAll(pred, "@pid::text", "$5::text")
		args = append(args, strings.TrimPrefix(view, "user:"))
	}
	query := fmt.Sprintf(previewQuery, pred)
	rows, err := d.Pool.Query(ctx, query, args...)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch previews")
		return
	}
	defer rows.Close()
	var page []previewRow
	for rows.Next() {
		var p previewRow
		var providerID, name, snippet, senderEmail, senderName, senderPhoto pgtype.Text
		var viewedAt, sortTs pgtype.Timestamptz
		var id, linkID pgtype.UUID
		var createdAt, updatedAt pgtype.Timestamptz
		if err := rows.Scan(&id, &providerID, &linkID, &p.InboxVisible, &p.IsRead,
			&p.OwnerID, &p.IsDraft, &p.IsImportant, &name, &snippet,
			&senderEmail, &senderName, &senderPhoto, &sortTs, &viewedAt,
			&createdAt, &updatedAt); err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to scan previews")
			return
		}
		p.ID = uuidFromPg(id)
		p.ProviderID = strPtrFromPg(providerID)
		p.LinkID = uuidFromPg(linkID)
		p.Name = strPtrFromPg(name)
		p.Snippet = strPtrFromPg(snippet)
		p.SenderEmail = strPtrFromPg(senderEmail)
		p.SenderName = strPtrFromPg(senderName)
		p.SenderPhotoURL = strPtrFromPg(senderPhoto)
		p.SortTs = tsFromPg(sortTs)
		p.ViewedAt = tsPtrFromPg(viewedAt)
		p.CreatedAt = tsFromPg(createdAt)
		p.UpdatedAt = tsFromPg(updatedAt)
		page = append(page, p)
	}
	if err := rows.Err(); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to fetch previews")
		return
	}

	var next *string
	if len(page) > limit {
		page = page[:limit]
		last := page[len(page)-1]
		cur := base64.StdEncoding.EncodeToString([]byte(
			last.SortTs.UTC().Format(time.RFC3339Nano) + "|" + last.ID.String()))
		next = &cur
	}

	// enrich: attachments + participants + labels per thread
	threadIDs := make([]pgtype.UUID, 0, len(page))
	for _, p := range page {
		threadIDs = append(threadIDs, pgUUID(p.ID))
	}
	attByThread, conByThread, lblByThread, err := d.previewEnrich(ctx, threadIDs)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to enrich previews")
		return
	}

	items := make([]ThreadPreview, 0, len(page))
	for _, p := range page {
		items = append(items, ThreadPreview{
			ID: p.ID, ProviderID: p.ProviderID, OwnerID: p.OwnerID,
			InboxVisible: p.InboxVisible, IsRead: p.IsRead, IsDraft: p.IsDraft,
			IsImportant: p.IsImportant, Name: p.Name, Snippet: p.Snippet,
			SenderEmail: p.SenderEmail, SenderName: p.SenderName,
			SenderPhotoURL: p.SenderPhotoURL, SortTs: p.SortTs,
			ViewedAt: p.ViewedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
			LinkID:      p.LinkID,
			Attachments: attByThread[p.ID], Participants: conByThread[p.ID],
			Labels: lblByThread[p.ID],
		})
	}
	httpx.WriteJSON(w, http.StatusOK, PaginatedThreadCursor{Items: items, NextCursor: next})
}

// previewEnrich loads attachments, participants, and labels for a page of
// threads. Attachments use the generated bulk query; contacts and labels use
// small scoped queries (no generated thread-scoped variant exists for the
// contact/label preview shape).
func (d *Deps) previewEnrich(ctx context.Context, threadIDs []pgtype.UUID) (
	map[uuid.UUID][]MessageAttachment,
	map[uuid.UUID][]PreviewContact,
	map[uuid.UUID][]Label,
	error,
) {
	atts := map[uuid.UUID][]MessageAttachment{}
	cons := map[uuid.UUID][]PreviewContact{}
	lbls := map[uuid.UUID][]Label{}
	if len(threadIDs) == 0 {
		return atts, cons, lbls, nil
	}

	arows, err := d.q().GetAttachmentsByThreadIds(ctx, threadIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, a := range arows {
		tid := uuidFromPg(a.ThreadID)
		atts[tid] = append(atts[tid], attachmentFromThreadRow(a))
	}

	crows, err := d.Pool.Query(ctx, `
		SELECT m.thread_id, c.id, c.link_id,
		       COALESCE(m.from_name, c.name) AS name,
		       c.email_address, c.sfs_photo_url
		FROM email_messages m
		JOIN email_contacts c ON m.from_contact_id = c.id
		WHERE m.thread_id = ANY($1::uuid[]) AND m.from_contact_id IS NOT NULL
		ORDER BY m.created_at ASC`, threadIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var tid, id, linkID pgtype.UUID
		var name, email, photo pgtype.Text
		if err := crows.Scan(&tid, &id, &linkID, &name, &email, &photo); err != nil {
			return nil, nil, nil, err
		}
		t := uuidFromPg(tid)
		cons[t] = append(cons[t], PreviewContact{
			ID: uuidFromPg(id), LinkID: uuidFromPg(linkID),
			Name: strPtrFromPg(name), EmailAddress: strPtrFromPg(email),
			SfsPhotoURL: strPtrFromPg(photo),
		})
	}
	crows.Close()

	lrows, err := d.Pool.Query(ctx, `
		SELECT DISTINCT ON (l.id, m.thread_id)
		       l.id, m.thread_id, l.link_id, l.provider_label_id, l.name,
		       l.created_at, l.message_list_visibility, l.label_list_visibility, l.type
		FROM email_messages m
		JOIN email_message_labels ml ON m.id = ml.message_id
		JOIN email_labels l ON ml.label_id = l.id
		WHERE m.thread_id = ANY($1::uuid[])`, threadIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var id, tid, linkID pgtype.UUID
		var pid, name string
		var created pgtype.Timestamptz
		var mlv, llv, typ string
		if err := lrows.Scan(&id, &tid, &linkID, &pid, &name, &created, &mlv, &llv, &typ); err != nil {
			return nil, nil, nil, err
		}
		t := uuidFromPg(tid)
		lbls[t] = append(lbls[t], Label{
			ID: uuidFromPg(id), LinkID: uuidFromPg(linkID),
			ProviderLabelID: pid, Name: name, CreatedAt: tsFromPg(created),
			MessageListVisibility: mlv, LabelListVisibility: llv, Type: typ,
		})
	}
	return atts, cons, lbls, lrows.Err()
}
