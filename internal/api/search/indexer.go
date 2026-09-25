package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgsearch "github.com/macro-inc/macro/pkg/search"
)

// ContentExtractor pulls full text out of binary/blob documents (docx/pdf in
// MinIO, email bodies, file attachments). It replaces the Rust
// s3+document-text-extractor+lexical pipeline.
type ContentExtractor interface {
	// ExtractDocumentText returns the searchable text for one document
	// version. Implementations may return ErrExtractionUnavailable when the
	// extractor pipeline isn't wired yet.
	ExtractDocumentText(ctx context.Context, msg SearchExtractorMessage) (string, error)
}

// ErrExtractionUnavailable marks the not-yet-ported extraction pipeline.
var ErrExtractionUnavailable = errors.New("search: content extraction not yet ported")

// StubContentExtractor is the default until the MinIO/lexical extraction
// port lands.
// TODO(extraction): port process::document (S3 fetch + lexical/docx/pdf
// extraction via the convert service or a dedicated extractor worker).
type StubContentExtractor struct{}

// ExtractDocumentText implements ContentExtractor.
func (StubContentExtractor) ExtractDocumentText(context.Context, SearchExtractorMessage) (string, error) {
	return "", ErrExtractionUnavailable
}

// Indexer processes search queue messages into the SearchPort index —
// replaces process::process_message + the per-entity handlers.
type Indexer struct {
	Store     pkgsearch.SearchPort
	Macro     *pgxpool.Pool
	Comms     *pgxpool.Pool // optional; channel messages fail without it
	Email     *pgxpool.Pool // optional; email threads fail without it
	Extractor ContentExtractor
	Embedder  pkgsearch.Embedder // optional; nil → no embeddings
}

// NewIndexer builds the processor.
func NewIndexer(store pkgsearch.SearchPort, macro, comms, email *pgxpool.Pool, extractor ContentExtractor) *Indexer {
	if extractor == nil {
		extractor = StubContentExtractor{}
	}
	return &Indexer{Store: store, Macro: macro, Comms: comms, Email: email, Extractor: extractor}
}

// Process dispatches one queue message (process::process_message).
// Unknown kinds are logged and dropped — forward-compatible with producers
// running a newer Rust service.
func (i *Indexer) Process(ctx context.Context, m QueueMessage) error {
	switch p := m.Payload.(type) {
	case SearchExtractorMessage:
		return i.indexDocument(ctx, p)
	case ChatMessagePayload:
		return i.indexChatMessage(ctx, p)
	case EmailThreadBatchMessage:
		return i.indexEmailThreads(ctx, p)
	case ChannelMessageUpdate:
		return i.indexChannelMessage(ctx, p)
	case CallRecordMessage:
		return i.indexCall(ctx, p)
	case RemoveCallRecord:
		return i.removeCall(ctx, p)
	case UpsertProject:
		return i.indexProject(ctx, p)
	case UpsertCalendarEvent:
		return i.indexCalendarEvent(ctx, p)
	case string:
		// RemoveUserProfile(String)
		return i.Store.Delete(ctx, pkgsearch.EntityUser, p)
	default:
		slog.Warn("search: unhandled queue message", "kind", m.Kind)
		return nil
	}
}

// embedIfAvailable produces a query-time embedding; failures degrade to
// keyword indexing.
func (i *Indexer) embedIfAvailable(ctx context.Context, text string) []float32 {
	if i.Embedder == nil || text == "" {
		return nil
	}
	v, err := i.Embedder.Embed(ctx, text)
	if err != nil {
		return nil
	}
	return v
}

// indexDocument resolves the document row, extracts content (stubbed
// extractor → title-only index), and upserts. Replaces
// process::document::extract_text + opensearch upsert.
func (i *Indexer) indexDocument(ctx context.Context, msg SearchExtractorMessage) error {
	var name, owner, fileType string
	var deletedAt *time.Time
	err := i.Macro.QueryRow(ctx, `
		SELECT name, owner, "fileType", "deletedAt" FROM "Document" WHERE id = $1`,
		msg.DocumentID).Scan(&name, &owner, &fileType, &deletedAt)
	if err == pgx.ErrNoRows {
		// Document gone — ensure it isn't indexed.
		return i.Store.Delete(ctx, pkgsearch.EntityDocument, msg.DocumentID)
	}
	if err != nil {
		return fmt.Errorf("search: load document %s: %w", msg.DocumentID, err)
	}
	if deletedAt != nil {
		return i.Store.Delete(ctx, pkgsearch.EntityDocument, msg.DocumentID)
	}
	content, err := i.Extractor.ExtractDocumentText(ctx, msg)
	if err != nil {
		if errors.Is(err, ErrExtractionUnavailable) {
			// Extraction pipeline not ported yet — index metadata only so
			// name search keeps working.
			slog.Debug("search: extraction unavailable, indexing metadata only",
				"document_id", msg.DocumentID)
		} else {
			return fmt.Errorf("search: extract %s: %w", msg.DocumentID, err)
		}
	}
	return i.Store.Index(ctx, []pkgsearch.Document{{
		EntityID:   msg.DocumentID,
		EntityType: pkgsearch.EntityDocument,
		OwnerID:    owner,
		Title:      name,
		Content:    content,
		Metadata:   map[string]any{"file_type": fileType, "document_version_id": msg.DocumentVersionID},
		Embedding:  i.embedIfAvailable(ctx, name+" "+content),
	}})
}

// chatMessageText extracts display text from the "ChatMessage".content jsonb
// (ChatMessageContent: plain string | assistant parts array).
func chatMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		// AssistantMessagePart::Text { text } serializes as {"Text":{"text":…}}
		// (externally tagged) or {"type":"Text","text":…} depending on serde
		// flavor — handle both.
		if t, ok := part["text"]; ok {
			var s string
			if json.Unmarshal(t, &s) == nil {
				b.WriteString(s)
			}
			continue
		}
		if t, ok := part["Text"]; ok {
			var inner struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(t, &inner) == nil {
				b.WriteString(inner.Text)
			}
		}
	}
	return b.String()
}

// indexChatMessage resolves the message row and upserts it.
func (i *Indexer) indexChatMessage(ctx context.Context, msg ChatMessagePayload) error {
	var content json.RawMessage
	var owner string
	var deletedAt *time.Time
	err := i.Macro.QueryRow(ctx, `
		SELECT m.content, c."userId", c."deletedAt"
		FROM "ChatMessage" m JOIN "Chat" c ON c."id" = m."chatId"
		WHERE m.id = $1`, msg.MessageID).Scan(&content, &owner, &deletedAt)
	if err == pgx.ErrNoRows || deletedAt != nil {
		return i.Store.Delete(ctx, pkgsearch.EntityChatMessage, msg.MessageID)
	}
	if err != nil {
		return fmt.Errorf("search: load chat message %s: %w", msg.MessageID, err)
	}
	text := chatMessageText(content)
	return i.Store.Index(ctx, []pkgsearch.Document{{
		EntityID:   msg.MessageID,
		EntityType: pkgsearch.EntityChatMessage,
		OwnerID:    owner,
		Content:    text,
		Metadata:   map[string]any{"chat_id": msg.ChatID},
		UpdatedAt:  msg.UpdatedAt,
		Embedding:  i.embedIfAvailable(ctx, text),
	}})
}

// indexChannelMessage resolves the comms_messages row and upserts it —
// mirrors channel::process_channel_message_update.
func (i *Indexer) indexChannelMessage(ctx context.Context, msg ChannelMessageUpdate) error {
	if i.Comms == nil {
		return errCommsDBMissing
	}
	channelID, err := uuid.Parse(msg.ChannelID)
	if err != nil {
		return fmt.Errorf("search: bad channel_id %q: %w", msg.ChannelID, err)
	}
	messageID, err := uuid.Parse(msg.MessageID)
	if err != nil {
		return fmt.Errorf("search: bad message_id %q: %w", msg.MessageID, err)
	}
	var channelName, senderID, content string
	var updatedAt, deletedAt *time.Time
	err = i.Comms.QueryRow(ctx, `
		SELECT c.name, m.sender_id, m.content, m.updated_at, m.deleted_at::timestamptz
		FROM comms_messages m
		JOIN comms_channels c ON c.id = m.channel_id
		WHERE m.id = $1 AND c.id = $2`, messageID, channelID).
		Scan(&channelName, &senderID, &content, &updatedAt, &deletedAt)
	if err == pgx.ErrNoRows || deletedAt != nil {
		return i.Store.Delete(ctx, pkgsearch.EntityChannelMessage, msg.MessageID)
	}
	if err != nil {
		return fmt.Errorf("search: load channel message %s: %w", msg.MessageID, err)
	}
	doc := pkgsearch.Document{
		EntityID:   msg.MessageID,
		EntityType: pkgsearch.EntityChannelMessage,
		OwnerID:    senderID,
		Title:      channelName,
		Content:    content,
		Metadata:   map[string]any{"channel_id": msg.ChannelID, "channel_name": channelName},
		Embedding:  i.embedIfAvailable(ctx, content),
	}
	if updatedAt != nil {
		doc.UpdatedAt = *updatedAt
	}
	return i.Store.Index(ctx, []pkgsearch.Document{doc})
}

// indexEmailThreads indexes each thread in the batch — the Rust worker
// fetched each thread's messages and wrote one OpenSearch doc per thread
// keyed by (macro_user_id, thread_id). We index subject + concatenated
// snippet/body text of the thread's messages.
// TODO(email-parity): the Rust email indexer also folded attachments,
// participants, and email-recipient metadata into the search doc; port the
// full shape when email search parity is needed.
func (i *Indexer) indexEmailThreads(ctx context.Context, msg EmailThreadBatchMessage) error {
	if i.Email == nil {
		return errEmailDBMissing
	}
	for _, tid := range msg.ThreadIDs {
		threadID, err := uuid.Parse(tid)
		if err != nil {
			return fmt.Errorf("search: bad thread id %q: %w", tid, err)
		}
		if err := i.indexEmailThread(ctx, threadID, msg.MacroUserID); err != nil {
			return err
		}
	}
	return nil
}

func (i *Indexer) indexEmailThread(ctx context.Context, threadID uuid.UUID, macroUserID string) error {
	rows, err := i.Email.Query(ctx, `
		SELECT COALESCE(m.subject, ''), COALESCE(m.snippet, ''), COALESCE(m.body_text, '')
		FROM email_messages m
		WHERE m.thread_id = $1
		ORDER BY m.internal_date_ts ASC
		LIMIT 50`, threadID)
	if err != nil {
		return fmt.Errorf("search: load thread %s: %w", threadID, err)
	}
	defer rows.Close()
	var subject string
	var bodies strings.Builder
	var found bool
	for rows.Next() {
		found = true
		var subj, snippet, body string
		if err := rows.Scan(&subj, &snippet, &body); err != nil {
			return fmt.Errorf("search: scan email message: %w", err)
		}
		if subject == "" {
			subject = subj
		}
		if body != "" {
			bodies.WriteString(body)
			bodies.WriteString("\n")
		} else {
			bodies.WriteString(snippet)
			bodies.WriteString("\n")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		return i.Store.Delete(ctx, pkgsearch.EntityEmailThread, threadID.String())
	}
	content := bodies.String()
	return i.Store.Index(ctx, []pkgsearch.Document{{
		EntityID:   threadID.String(),
		EntityType: pkgsearch.EntityEmailThread,
		OwnerID:    macroUserID,
		Title:      subject,
		Content:    content,
		Metadata:   map[string]any{"macro_user_id": macroUserID},
		Embedding:  i.embedIfAvailable(ctx, subject+" "+content),
	}})
}

// indexCall indexes a call record by its searchable name — mirrors
// process::call::process_call_record (title = custom_name ?? channel_name).
// TODO(call-parity): the Rust indexer pulled the transcript/summary text
// from the call record too; port when call transcript storage is in Go.
func (i *Indexer) indexCall(ctx context.Context, msg CallRecordMessage) error {
	callID, err := uuid.Parse(msg.CallID)
	if err != nil {
		return fmt.Errorf("search: bad call id %q: %w", msg.CallID, err)
	}
	var customName, channelName *string
	var startedAt time.Time
	var createdBy string
	err = i.Macro.QueryRow(ctx, `
		SELECT custom_name, channel_name, started_at, created_by
		FROM call_records WHERE id = $1`, callID).
		Scan(&customName, &channelName, &startedAt, &createdBy)
	if err == pgx.ErrNoRows {
		return i.Store.Delete(ctx, pkgsearch.EntityCall, msg.CallID)
	}
	if err != nil {
		return fmt.Errorf("search: load call %s: %w", msg.CallID, err)
	}
	title := ""
	if customName != nil {
		title = *customName
	} else if channelName != nil {
		title = *channelName
	}
	return i.Store.Index(ctx, []pkgsearch.Document{{
		EntityID:   msg.CallID,
		EntityType: pkgsearch.EntityCall,
		OwnerID:    createdBy,
		Title:      title,
		Metadata:   map[string]any{"started_at": startedAt},
		UpdatedAt:  startedAt,
		Embedding:  i.embedIfAvailable(ctx, title),
	}})
}

// removeCall deletes one call (or every call on a channel when call_id is
// null — mirrors RemoveCallRecord semantics).
func (i *Indexer) removeCall(ctx context.Context, msg RemoveCallRecord) error {
	if msg.CallID != nil {
		return i.Store.Delete(ctx, pkgsearch.EntityCall, *msg.CallID)
	}
	// Channel-scoped removal: delete every call_records row for the channel.
	rows, err := i.Macro.Query(ctx,
		`SELECT id FROM call_records WHERE channel_id = $1`, msg.ChannelID)
	if err != nil {
		return fmt.Errorf("search: list calls for channel %s: %w", msg.ChannelID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id.String())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := i.Store.Delete(ctx, pkgsearch.EntityCall, id); err != nil {
			return err
		}
	}
	return nil
}

// indexProject indexes the project name — mirrors
// project::process_project (name + user metadata only in OpenSearch).
func (i *Indexer) indexProject(ctx context.Context, msg UpsertProject) error {
	var name, owner string
	var deletedAt *time.Time
	var updatedAt time.Time
	err := i.Macro.QueryRow(ctx, `
		SELECT name, "userId", "updatedAt"::timestamptz, "deletedAt"::timestamptz
		FROM "Project" WHERE id = $1`, msg.ProjectID).
		Scan(&name, &owner, &updatedAt, &deletedAt)
	if err == pgx.ErrNoRows || deletedAt != nil {
		return i.Store.Delete(ctx, pkgsearch.EntityProject, msg.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("search: load project %s: %w", msg.ProjectID, err)
	}
	return i.Store.Index(ctx, []pkgsearch.Document{{
		EntityID:   msg.ProjectID,
		EntityType: pkgsearch.EntityProject,
		OwnerID:    owner,
		Title:      name,
		UpdatedAt:  updatedAt,
		Embedding:  i.embedIfAvailable(ctx, name),
	}})
}

// indexCalendarEvent indexes the series-master event — mirrors
// calendar_event::process_calendar_event (title + description + location).
func (i *Indexer) indexCalendarEvent(ctx context.Context, msg UpsertCalendarEvent) error {
	eventID, err := uuid.Parse(msg.EventID)
	if err != nil {
		return fmt.Errorf("search: bad event id %q: %w", msg.EventID, err)
	}
	var ownerID, title string
	var description, location *string
	var updatedAt time.Time
	err = i.Macro.QueryRow(ctx, `
		SELECT owner_id, COALESCE(title, ''), description, location, updated_at
		FROM calendar_events WHERE id = $1`, eventID).
		Scan(&ownerID, &title, &description, &location, &updatedAt)
	if err == pgx.ErrNoRows {
		return i.Store.Delete(ctx, pkgsearch.EntityCalendarEvent, msg.EventID)
	}
	if err != nil {
		return fmt.Errorf("search: load calendar event %s: %w", msg.EventID, err)
	}
	content := ""
	if description != nil {
		content = *description
	}
	if location != nil && *location != "" {
		content += " " + *location
	}
	return i.Store.Index(ctx, []pkgsearch.Document{{
		EntityID:   msg.EventID,
		EntityType: pkgsearch.EntityCalendarEvent,
		OwnerID:    ownerID,
		Title:      title,
		Content:    content,
		UpdatedAt:  updatedAt,
		Embedding:  i.embedIfAvailable(ctx, title+" "+content),
	}})
}

// PgPropertyIndexer re-fetches one entity's denormalized property rows and
// merges them into its indexed metadata — replaces
// DirectPropertyBackfillIndexer's OpenSearch-side property update.
type PgPropertyIndexer struct {
	Indexer *Indexer
}

// Reindex implements PropertyBackfillIndexer.
func (p PgPropertyIndexer) Reindex(ctx context.Context, entityID, entityType string) error {
	// Pull flattened property values (property_definitions display_name +
	// entity_properties values) and merge into the search doc's metadata.
	rows, err := p.Indexer.Macro.Query(ctx, `
		SELECT pd.display_name, ep.values
		FROM entity_properties ep
		JOIN property_definitions pd ON pd.id = ep.property_definition_id
		WHERE ep.entity_id = $1 AND ep.entity_type::text = $2`, entityID, entityType)
	if err != nil {
		return fmt.Errorf("search: load properties for %s/%s: %w", entityType, entityID, err)
	}
	defer rows.Close()
	props := map[string]any{}
	for rows.Next() {
		var name string
		var vals json.RawMessage
		if err := rows.Scan(&name, &vals); err != nil {
			return fmt.Errorf("search: scan property: %w", err)
		}
		var v any
		if json.Unmarshal(vals, &v) == nil {
			props[name] = v
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// TODO(properties-parity): the Rust indexer overwrote only the
	// `properties` field of the existing OpenSearch doc. Ours JSONB-merges
	// into `metadata.properties`; a full doc upsert from the entity's normal
	// path keeps the shape consistent.
	return p.Indexer.Store.MergeMetadata(ctx, entityType, entityID,
		map[string]any{"properties": props})
}
