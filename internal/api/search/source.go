package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SourcePage is one page of backfill work: the messages to publish plus the
// source rows consumed (they differ when rows fold into fewer messages).
type SourcePage struct {
	Messages     []QueueMessage
	RowsConsumed int
}

// PropertySourcePage is one page of entities whose denormalized properties
// get reindexed directly (no queue hop).
type PropertySourcePage struct {
	EntityIDs    []string
	EntityType   string
	RowsConsumed int
}

// Keyset cursors (updated_at, id) — opaque per entity.
type docCursor struct {
	UpdatedAt  time.Time
	DocumentID string
}

type chatCursor struct {
	UpdatedAt time.Time
	MessageID string
}

type projectCursor struct {
	UpdatedAt time.Time
	ProjectID string
}

type calendarCursor struct {
	UpdatedAt time.Time
	EventID   uuid.UUID
}

type callCursor struct {
	StartedAt time.Time
	CallID    uuid.UUID
}

// PageSizes bounds each per-entity fetch (mirrors config.backfill_page_sizes).
// EmailBatch bounds thread ids per queue message (DEFAULT_EMAIL_BATCH_SIZE).
type PageSizes struct {
	Documents      int
	Chats          int
	Channels       int
	Emails         int
	Projects       int
	Calls          int
	CalendarEvents int
	Properties     int
	EmailBatch     int
}

// DefaultPageSizes matches the Rust defaults.
func DefaultPageSizes() PageSizes {
	return PageSizes{
		Documents:      500,
		Chats:          500,
		Channels:       500,
		Emails:         500,
		Projects:       500,
		Calls:          500,
		CalendarEvents: 500,
		Properties:     500,
		EmailBatch:     20,
	}
}

// PgBackfillSource is the production BackfillSource: reads the searchable
// tables directly (macrodb + commsdb + emaildb pools; commsdb/emaildb are
// optional — the channel/email fetchers fail clearly without them).
type PgBackfillSource struct {
	macro *pgxpool.Pool
	comms *pgxpool.Pool
	email *pgxpool.Pool
	sizes PageSizes
}

// NewPgBackfillSource builds the source over the three pools.
func NewPgBackfillSource(macro, comms, email *pgxpool.Pool, sizes PageSizes) *PgBackfillSource {
	if sizes == (PageSizes{}) {
		sizes = DefaultPageSizes()
	}
	return &PgBackfillSource{macro: macro, comms: comms, email: email, sizes: sizes}
}

var errCommsDBMissing = errors.New("search: commsdb pool not configured")
var errEmailDBMissing = errors.New("search: emaildb pool not configured")

// FetchDocuments pages "Document" by (updatedAt, id) keyset — mirrors
// get_documents_for_search including the version-id laterals.
func (s *PgBackfillSource) FetchDocuments(
	ctx context.Context, req DocumentBackfillRequest, cursor *docCursor,
) (SourcePage, *docCursor, error) {
	var cursorAt *time.Time
	var cursorID *string
	if cursor != nil {
		cursorAt, cursorID = &cursor.UpdatedAt, &cursor.DocumentID
	}
	var fileTypes []string
	if len(req.FileTypes) > 0 {
		fileTypes = req.FileTypes
	}
	rows, err := s.macro.Query(ctx, `
		SELECT
			d.id AS document_id,
			d.owner AS owner,
			d."fileType" AS file_type,
			COALESCE(db.id, di.id, dipdf.id) AS document_version_id,
			d."updatedAt"::timestamptz AS updated_at
		FROM "Document" d
		LEFT JOIN document_sub_type dst ON dst.document_id = d.id
		LEFT JOIN LATERAL (
			SELECT b.id FROM "DocumentBom" b
			WHERE b."documentId" = d.id
			ORDER BY b."createdAt" DESC LIMIT 1
		) db ON d."fileType" = 'docx'
		LEFT JOIN LATERAL (
			SELECT i.id FROM "DocumentInstance" i
			WHERE i."documentId" = d.id
			ORDER BY i."updatedAt" ASC LIMIT 1
		) dipdf ON d."fileType" = 'pdf'
		LEFT JOIN LATERAL (
			SELECT i.id FROM "DocumentInstance" i
			WHERE i."documentId" = d.id
			ORDER BY i."createdAt" DESC LIMIT 1
		) di ON d."fileType" IS DISTINCT FROM 'docx' AND d."fileType" IS DISTINCT FROM 'pdf'
		WHERE d."fileType" IS NOT NULL
			AND ($3::text[] IS NULL OR d."fileType" = ANY($3))
			AND ($4::text IS NULL OR dst.sub_type::text = $4)
			AND ($5::timestamptz IS NULL OR d."updatedAt" >= $5)
			AND ($6::timestamptz IS NULL OR d."updatedAt" < $6)
			AND (
				$7::bool IS NULL
				OR ($7 AND d."deletedAt" IS NOT NULL)
				OR (NOT $7 AND d."deletedAt" IS NULL)
			)
			AND (
				$2::timestamptz IS NULL
				OR (d."updatedAt", d.id) > ($2, $8::text)
			)
		ORDER BY d."updatedAt" ASC, d.id ASC
		LIMIT $1`,
		s.sizes.Documents, cursorAt, fileTypes, req.SubType,
		req.UpdatedAfter, req.UpdatedBefore, req.DeletionFilter.onlyDeleted(), cursorID)
	if err != nil {
		return SourcePage{}, nil, fmt.Errorf("search: fetch documents: %w", err)
	}
	defer rows.Close()

	page := SourcePage{}
	var next *docCursor
	for rows.Next() {
		var (
			docID, owner, fileType string
			versionID              *string
			updatedAt              time.Time
		)
		if err := rows.Scan(&docID, &owner, &fileType, &versionID, &updatedAt); err != nil {
			return SourcePage{}, nil, fmt.Errorf("search: scan document: %w", err)
		}
		msg := SearchExtractorMessage{
			UserID:            owner,
			DocumentID:        docID,
			FileType:          fileType,
			DocumentVersionID: versionID,
			IndexOverride:     req.IndexOverride,
		}
		kind := KindExtractDocumentText
		if fileType == "md" {
			kind = KindExtractSync
		}
		page.Messages = append(page.Messages, QueueMessage{Kind: kind, Payload: msg})
		page.RowsConsumed++
		next = &docCursor{UpdatedAt: updatedAt, DocumentID: docID}
	}
	return page, next, rows.Err()
}

// FetchChats pages "ChatMessage" joined to "Chat" by (updatedAt, id) keyset
// — mirrors get_chat_messages_for_search_backfill (chat_ids + user_ids
// filters folded into ANY clauses).
func (s *PgBackfillSource) FetchChats(
	ctx context.Context, req ChatBackfillRequest, cursor *chatCursor,
) (SourcePage, *chatCursor, error) {
	var cursorAt *time.Time
	var cursorID *string
	if cursor != nil {
		cursorAt, cursorID = &cursor.UpdatedAt, &cursor.MessageID
	}
	rows, err := s.macro.Query(ctx, `
		SELECT
			c."id" AS chat_id,
			m.id AS message_id,
			c."userId" AS user_id,
			m."createdAt" AS created_at,
			m."updatedAt" AS updated_at
		FROM "ChatMessage" m
		JOIN "Chat" c ON c."id" = m."chatId"
		WHERE (
				$2::bool IS NULL
				OR ($2 AND c."deletedAt" IS NOT NULL)
				OR (NOT $2 AND c."deletedAt" IS NULL)
			)
			AND ($3::timestamptz IS NULL OR m."updatedAt" >= $3)
			AND ($4::timestamptz IS NULL OR m."updatedAt" < $4)
			AND (
				$5::timestamptz IS NULL
				OR (m."updatedAt", m.id) > ($5, $6::text)
			)
			AND (cardinality($7::text[]) = 0 OR m."chatId" = ANY($7))
			AND (cardinality($8::text[]) = 0 OR c."userId" = ANY($8))
		ORDER BY m."updatedAt" ASC, m.id ASC
		LIMIT $1`,
		s.sizes.Chats, req.DeletionFilter.onlyDeleted(),
		req.UpdatedAfter, req.UpdatedBefore, cursorAt, cursorID,
		req.ChatIDs, req.UserIDs)
	if err != nil {
		return SourcePage{}, nil, fmt.Errorf("search: fetch chats: %w", err)
	}
	defer rows.Close()

	page := SourcePage{}
	var next *chatCursor
	for rows.Next() {
		var m ChatMessagePayload
		if err := rows.Scan(&m.ChatID, &m.MessageID, &m.UserID, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return SourcePage{}, nil, fmt.Errorf("search: scan chat message: %w", err)
		}
		m.IndexOverride = req.IndexOverride
		page.Messages = append(page.Messages, QueueMessage{Kind: KindChatMessage, Payload: m})
		page.RowsConsumed++
		next = &chatCursor{UpdatedAt: m.UpdatedAt, MessageID: m.MessageID}
	}
	return page, next, rows.Err()
}

// FetchChannels pages comms_messages by offset — mirrors
// get_channel_messages.
func (s *PgBackfillSource) FetchChannels(
	ctx context.Context, req ChannelBackfillRequest, offset int,
) (SourcePage, error) {
	if s.comms == nil {
		return SourcePage{}, errCommsDBMissing
	}
	rows, err := s.comms.Query(ctx, `
		SELECT channel_id, id
		FROM comms_messages
		WHERE channel_id IS NOT NULL
			AND (
				$3::bool IS NULL
				OR ($3 AND deleted_at IS NOT NULL)
				OR (NOT $3 AND deleted_at IS NULL)
			)
		ORDER BY created_at ASC
		LIMIT $1 OFFSET $2`,
		s.sizes.Channels, offset, req.DeletionFilter.onlyDeleted())
	if err != nil {
		return SourcePage{}, fmt.Errorf("search: fetch channel messages: %w", err)
	}
	defer rows.Close()

	page := SourcePage{}
	for rows.Next() {
		var channelID, messageID uuid.UUID
		if err := rows.Scan(&channelID, &messageID); err != nil {
			return SourcePage{}, fmt.Errorf("search: scan channel message: %w", err)
		}
		page.Messages = append(page.Messages, QueueMessage{
			Kind: KindChannelMessage,
			Payload: ChannelMessageUpdate{
				ChannelID:     channelID.String(),
				MessageID:     messageID.String(),
				IndexOverride: req.IndexOverride,
			},
		})
		page.RowsConsumed++
	}
	return page, rows.Err()
}

// FetchEmails pages email_threads by offset, folding thread ids into
// per-user batches — mirrors fetch_emails + email_source_page.
func (s *PgBackfillSource) FetchEmails(
	ctx context.Context, req EmailBackfillRequest, offset int,
) (SourcePage, error) {
	if s.email == nil {
		return SourcePage{}, errEmailDBMissing
	}
	batchSize := s.sizes.EmailBatch
	if batchSize <= 0 {
		batchSize = 20
	}
	if req.BatchSize != nil && *req.BatchSize > 0 {
		batchSize = *req.BatchSize
	}

	// Explicit thread-id list: page the ids in memory (primary-key lookups).
	if len(req.ThreadIDs) > 0 {
		start := min(offset, len(req.ThreadIDs))
		end := min(start+s.sizes.Emails, len(req.ThreadIDs))
		pageIDs := req.ThreadIDs[start:end]
		if len(pageIDs) == 0 {
			return SourcePage{}, nil
		}
		uuids := make([]uuid.UUID, 0, len(pageIDs))
		for _, id := range pageIDs {
			u, err := uuid.Parse(id)
			if err != nil {
				return SourcePage{}, fmt.Errorf("search: invalid thread id %q: %w", id, err)
			}
			uuids = append(uuids, u)
		}
		rows, err := s.email.Query(ctx, `
			SELECT t.id, l.macro_id
			FROM email_threads t
			JOIN email_links l ON t.link_id = l.id
			WHERE t.id = ANY($1::uuid[])`, uuids)
		if err != nil {
			return SourcePage{}, fmt.Errorf("search: fetch threads by id: %w", err)
		}
		batch, err := s.emailSourcePage(rows, batchSize, req.IndexOverride)
		batch.RowsConsumed = len(pageIDs) // advance by ids consumed, not rows found
		return batch, err
	}

	var rows pgx.Rows
	var err error
	if req.Since != nil {
		rows, err = s.email.Query(ctx, `
			SELECT t.id, l.macro_id
			FROM email_threads t
			JOIN email_links l ON t.link_id = l.id
			WHERE t.updated_at >= $3
			ORDER BY t.latest_inbound_message_ts DESC NULLS LAST
			LIMIT $1 OFFSET $2`, s.sizes.Emails, offset, *req.Since)
	} else {
		rows, err = s.email.Query(ctx, `
			SELECT t.id, l.macro_id
			FROM email_threads t
			JOIN email_links l ON t.link_id = l.id
			ORDER BY t.latest_inbound_message_ts DESC NULLS LAST
			LIMIT $1 OFFSET $2`, s.sizes.Emails, offset)
	}
	if err != nil {
		return SourcePage{}, fmt.Errorf("search: fetch threads: %w", err)
	}
	return s.emailSourcePage(rows, batchSize, req.IndexOverride)
}

// emailSourcePage groups (thread_id, macro_user_id) rows into per-user
// batches — mirrors the Rust email_source_page.
func (s *PgBackfillSource) emailSourcePage(
	rows pgx.Rows, batchSize int, indexOverride *string,
) (SourcePage, error) {
	defer rows.Close()
	byUser := map[string][]string{}
	var order []string
	consumed := 0
	for rows.Next() {
		var threadID uuid.UUID
		var macroUserID string
		if err := rows.Scan(&threadID, &macroUserID); err != nil {
			return SourcePage{}, fmt.Errorf("search: scan thread: %w", err)
		}
		if _, ok := byUser[macroUserID]; !ok {
			order = append(order, macroUserID)
		}
		byUser[macroUserID] = append(byUser[macroUserID], threadID.String())
		consumed++
	}
	page := SourcePage{RowsConsumed: consumed}
	for _, user := range order {
		ids := byUser[user]
		for i := 0; i < len(ids); i += batchSize {
			page.Messages = append(page.Messages, QueueMessage{
				Kind: KindEmailThreadBatch,
				Payload: EmailThreadBatchMessage{
					ThreadIDs:     ids[i:min(i+batchSize, len(ids))],
					MacroUserID:   user,
					IndexOverride: indexOverride,
				},
			})
		}
	}
	return page, rows.Err()
}

// FetchProjects pages "Project" by (updatedAt, id) keyset — mirrors
// get_projects_for_search_backfill.
func (s *PgBackfillSource) FetchProjects(
	ctx context.Context, req ProjectBackfillRequest, cursor *projectCursor,
) (SourcePage, *projectCursor, error) {
	var cursorAt *time.Time
	var cursorID *string
	if cursor != nil {
		cursorAt, cursorID = &cursor.UpdatedAt, &cursor.ProjectID
	}
	rows, err := s.macro.Query(ctx, `
		SELECT p.id AS project_id, p."updatedAt"::timestamptz AS updated_at
		FROM "Project" p
		WHERE p."deletedAt" IS NULL
			AND (
				$2::timestamptz IS NULL
				OR (p."updatedAt", p.id) > ($2, $3)
			)
			AND ($4::timestamptz IS NULL OR p."updatedAt" >= $4)
			AND ($5::timestamptz IS NULL OR p."updatedAt" <= $5)
		ORDER BY p."updatedAt" ASC, p.id ASC
		LIMIT $1`,
		s.sizes.Projects, cursorAt, cursorID, req.UpdatedAfter, req.UpdatedBefore)
	if err != nil {
		return SourcePage{}, nil, fmt.Errorf("search: fetch projects: %w", err)
	}
	defer rows.Close()

	page := SourcePage{}
	var next *projectCursor
	for rows.Next() {
		var id string
		var updatedAt time.Time
		if err := rows.Scan(&id, &updatedAt); err != nil {
			return SourcePage{}, nil, fmt.Errorf("search: scan project: %w", err)
		}
		page.Messages = append(page.Messages, QueueMessage{
			Kind:    KindUpsertProject,
			Payload: UpsertProject{ProjectID: id, IndexOverride: req.IndexOverride},
		})
		page.RowsConsumed++
		next = &projectCursor{UpdatedAt: updatedAt, ProjectID: id}
	}
	return page, next, rows.Err()
}

// FetchCalls pages call_records by (started_at, id) keyset, or the explicit
// call_ids list — mirrors fetch_calls + get_call_records_for_search_backfill.
func (s *PgBackfillSource) FetchCalls(
	ctx context.Context, req CallBackfillRequest, cursor *callCursor,
) (SourcePage, *callCursor, error) {
	if len(req.CallIDs) > 0 {
		// Explicit list: paginate it with the cursor by position. The Rust
		// version walks the same keyset over `call_records` restricted to the
		// ids, so we do the same.
		return s.fetchCallsByIDs(ctx, req, cursor)
	}
	var cursorAt *time.Time
	var cursorID *uuid.UUID
	if cursor != nil {
		cursorAt, cursorID = &cursor.StartedAt, &cursor.CallID
	}
	rows, err := s.macro.Query(ctx, `
		SELECT id AS call_id, started_at
		FROM call_records
		WHERE ($2::timestamptz IS NULL OR started_at >= $2)
			AND ($3::timestamptz IS NULL OR started_at < $3)
			AND (
				$4::timestamptz IS NULL
				OR (started_at, id) > ($4, $5::uuid)
			)
		ORDER BY started_at ASC, id ASC
		LIMIT $1`,
		s.sizes.Calls, req.StartedAfter, req.StartedBefore, cursorAt, cursorID)
	if err != nil {
		return SourcePage{}, nil, fmt.Errorf("search: fetch calls: %w", err)
	}
	defer rows.Close()
	return scanCallRows(rows, req.IndexOverride)
}

func (s *PgBackfillSource) fetchCallsByIDs(
	ctx context.Context, req CallBackfillRequest, cursor *callCursor,
) (SourcePage, *callCursor, error) {
	uuids := make([]uuid.UUID, 0, len(req.CallIDs))
	for _, id := range req.CallIDs {
		u, err := uuid.Parse(id)
		if err != nil {
			return SourcePage{}, nil, fmt.Errorf("search: invalid call id %q: %w", id, err)
		}
		uuids = append(uuids, u)
	}
	var cursorAt *time.Time
	var cursorID *uuid.UUID
	if cursor != nil {
		cursorAt, cursorID = &cursor.StartedAt, &cursor.CallID
	}
	rows, err := s.macro.Query(ctx, `
		SELECT id AS call_id, started_at
		FROM call_records
		WHERE id = ANY($6::uuid[])
			AND ($2::timestamptz IS NULL OR started_at >= $2)
			AND ($3::timestamptz IS NULL OR started_at < $3)
			AND (
				$4::timestamptz IS NULL
				OR (started_at, id) > ($4, $5::uuid)
			)
		ORDER BY started_at ASC, id ASC
		LIMIT $1`,
		s.sizes.Calls, req.StartedAfter, req.StartedBefore, cursorAt, cursorID, uuids)
	if err != nil {
		return SourcePage{}, nil, fmt.Errorf("search: fetch calls by id: %w", err)
	}
	defer rows.Close()
	return scanCallRows(rows, req.IndexOverride)
}

func scanCallRows(rows pgx.Rows, indexOverride *string) (SourcePage, *callCursor, error) {
	page := SourcePage{}
	var next *callCursor
	for rows.Next() {
		var id uuid.UUID
		var startedAt time.Time
		if err := rows.Scan(&id, &startedAt); err != nil {
			return SourcePage{}, nil, fmt.Errorf("search: scan call: %w", err)
		}
		page.Messages = append(page.Messages, QueueMessage{
			Kind:    KindCallRecord,
			Payload: CallRecordMessage{CallID: id.String(), IndexOverride: indexOverride},
		})
		page.RowsConsumed++
		next = &callCursor{StartedAt: startedAt, CallID: id}
	}
	return page, next, rows.Err()
}

// FetchCalendarEvents pages calendar_events by (updated_at, id) keyset —
// mirrors get_calendar_events_for_search_backfill (series masters only).
func (s *PgBackfillSource) FetchCalendarEvents(
	ctx context.Context, req CalendarEventBackfillRequest, cursor *calendarCursor,
) (SourcePage, *calendarCursor, error) {
	var cursorAt *time.Time
	var cursorID *uuid.UUID
	if cursor != nil {
		cursorAt, cursorID = &cursor.UpdatedAt, &cursor.EventID
	}
	rows, err := s.macro.Query(ctx, `
		SELECT event.id AS event_id, event.updated_at
		FROM calendar_events event
		WHERE ($2::timestamptz IS NULL OR $3::uuid IS NULL
				OR (event.updated_at, event.id) > ($2, $3))
			AND ($4::timestamptz IS NULL OR event.updated_at >= $4)
			AND ($5::timestamptz IS NULL OR event.updated_at < $5)
		ORDER BY event.updated_at ASC, event.id ASC
		LIMIT $1`,
		s.sizes.CalendarEvents, cursorAt, cursorID, req.UpdatedAfter, req.UpdatedBefore)
	if err != nil {
		return SourcePage{}, nil, fmt.Errorf("search: fetch calendar events: %w", err)
	}
	defer rows.Close()

	page := SourcePage{}
	var next *calendarCursor
	for rows.Next() {
		var id uuid.UUID
		var updatedAt time.Time
		if err := rows.Scan(&id, &updatedAt); err != nil {
			return SourcePage{}, nil, fmt.Errorf("search: scan calendar event: %w", err)
		}
		page.Messages = append(page.Messages, QueueMessage{
			Kind:    KindUpsertCalendarEvent,
			Payload: UpsertCalendarEvent{EventID: id.String(), IndexOverride: req.IndexOverride},
		})
		page.RowsConsumed++
		next = &calendarCursor{UpdatedAt: updatedAt, EventID: id}
	}
	return page, next, rows.Err()
}

// FetchEntityProperties pages distinct entity ids of one property type —
// mirrors get_entity_ids_with_properties.
func (s *PgBackfillSource) FetchEntityProperties(
	ctx context.Context, req PropertiesBackfillRequest, offset int,
) (PropertySourcePage, error) {
	if req.EntityType == "" {
		return PropertySourcePage{}, errors.New("search: entity_type required")
	}
	rows, err := s.macro.Query(ctx, `
		SELECT DISTINCT entity_id
		FROM entity_properties
		WHERE entity_type::text = $1
		ORDER BY entity_id
		LIMIT $2 OFFSET $3`,
		req.EntityType, s.sizes.Properties, offset)
	if err != nil {
		return PropertySourcePage{}, fmt.Errorf("search: fetch entity properties: %w", err)
	}
	defer rows.Close()

	page := PropertySourcePage{EntityType: req.EntityType}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return PropertySourcePage{}, fmt.Errorf("search: scan entity id: %w", err)
		}
		page.EntityIDs = append(page.EntityIDs, id)
		page.RowsConsumed++
	}
	return page, rows.Err()
}
