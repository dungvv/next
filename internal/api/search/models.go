// Package search ports services/search_processing_service to a chi
// sub-router: the internal backfill/extract/delete API, the JetStream
// consumer replacing the Kafka+SQS processing pipeline, and a Postgres FTS /
// pgvector index (pkg/search) replacing OpenSearch.
package search

import (
	"encoding/json"
	"fmt"
	"time"
)

// QueueMessage is the work-queue message shape, wire-compatible with the
// Rust sqs_client::search::SearchQueueMessage externally-tagged enum:
// {"Kind": {payload}} on the wire.
type QueueMessage struct {
	Kind    string
	Payload any
}

// Queue message kinds (serde enum variant names).
const (
	KindExtractDocumentText = "ExtractDocumentText"
	KindExtractSync         = "ExtractSync"
	KindChatMessage         = "ChatMessage"
	KindEmailThreadBatch    = "ExtractEmailThreadBatch"
	KindChannelMessage      = "ChannelMessageUpdate"
	KindCallRecord          = "CallRecord"
	KindRemoveCallRecord    = "RemoveCallRecord"
	KindUpsertProject       = "UpsertProject"
	KindUpsertCalendarEvent = "UpsertCalendarEvent"
	KindRemoveUserProfile   = "RemoveUserProfile"
)

// SearchExtractorMessage mirrors sqs_client::search::document::SearchExtractorMessage.
type SearchExtractorMessage struct {
	UserID            string  `json:"user_id"`
	DocumentID        string  `json:"document_id"`
	FileType          string  `json:"file_type"`
	DocumentVersionID *string `json:"document_version_id,omitempty"`
	IndexOverride     *string `json:"index_override,omitempty"`
}

// ChatMessagePayload mirrors sqs_client::search::chat::ChatMessage.
type ChatMessagePayload struct {
	ChatID        string    `json:"chat_id"`
	MessageID     string    `json:"message_id"`
	UserID        string    `json:"user_id"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	IndexOverride *string   `json:"index_override,omitempty"`
}

// EmailThreadBatchMessage mirrors sqs_client::search::email::EmailThreadBatchMessage.
type EmailThreadBatchMessage struct {
	ThreadIDs     []string `json:"thread_ids"`
	MacroUserID   string   `json:"macro_user_id"`
	IndexOverride *string  `json:"index_override,omitempty"`
}

// ChannelMessageUpdate mirrors sqs_client::search::channel::ChannelMessageUpdate.
type ChannelMessageUpdate struct {
	ChannelID     string  `json:"channel_id"`
	MessageID     string  `json:"message_id"`
	IndexOverride *string `json:"index_override,omitempty"`
}

// CallRecordMessage mirrors sqs_client::search::call::CallRecordMessage.
type CallRecordMessage struct {
	CallID        string  `json:"call_id"`
	IndexOverride *string `json:"index_override,omitempty"`
}

// RemoveCallRecord mirrors sqs_client::search::call::RemoveCallRecord.
type RemoveCallRecord struct {
	ChannelID     string  `json:"channel_id"`
	CallID        *string `json:"call_id,omitempty"`
	IndexOverride *string `json:"index_override,omitempty"`
}

// UpsertProject mirrors sqs_client::search::project::UpsertProject.
type UpsertProject struct {
	ProjectID     string  `json:"project_id"`
	IndexOverride *string `json:"index_override,omitempty"`
}

// UpsertCalendarEvent mirrors sqs_client::search::calendar_event::UpsertCalendarEvent.
type UpsertCalendarEvent struct {
	EventID       string  `json:"event_id"`
	IndexOverride *string `json:"index_override,omitempty"`
}

// MarshalJSON emits the externally-tagged enum form.
func (m QueueMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{m.Kind: m.Payload})
}

// UnmarshalJSON decodes the externally-tagged enum form.
func (m *QueueMessage) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 1 {
		return fmt.Errorf("search: queue message must have exactly one variant")
	}
	for kind, payload := range raw {
		m.Kind = kind
		var v any
		switch kind {
		case KindExtractDocumentText, KindExtractSync:
			v = &SearchExtractorMessage{}
		case KindChatMessage:
			v = &ChatMessagePayload{}
		case KindEmailThreadBatch:
			v = &EmailThreadBatchMessage{}
		case KindChannelMessage:
			v = &ChannelMessageUpdate{}
		case KindCallRecord:
			v = &CallRecordMessage{}
		case KindRemoveCallRecord:
			v = &RemoveCallRecord{}
		case KindUpsertProject:
			v = &UpsertProject{}
		case KindUpsertCalendarEvent:
			v = &UpsertCalendarEvent{}
		case KindRemoveUserProfile:
			v = new(string)
		default:
			// Unknown variants are kept as raw JSON so the consumer can
			// forward-compatibly ignore them.
			var generic any
			if err := json.Unmarshal(payload, &generic); err != nil {
				return err
			}
			m.Payload = generic
			return nil
		}
		if err := json.Unmarshal(payload, v); err != nil {
			return fmt.Errorf("search: decode %s payload: %w", kind, err)
		}
		m.Payload = v
	}
	return nil
}

// PrimaryID mirrors SearchQueueMessage::id() — the entity id used for
// subject sharding.
func (m QueueMessage) PrimaryID() string {
	switch p := m.Payload.(type) {
	case *SearchExtractorMessage:
		return p.DocumentID
	case SearchExtractorMessage:
		return p.DocumentID
	case *ChatMessagePayload:
		return p.MessageID
	case ChatMessagePayload:
		return p.MessageID
	case *EmailThreadBatchMessage:
		if len(p.ThreadIDs) > 0 {
			return p.ThreadIDs[0]
		}
	case EmailThreadBatchMessage:
		if len(p.ThreadIDs) > 0 {
			return p.ThreadIDs[0]
		}
	case *ChannelMessageUpdate:
		return p.MessageID
	case ChannelMessageUpdate:
		return p.MessageID
	case *CallRecordMessage:
		return p.CallID
	case CallRecordMessage:
		return p.CallID
	case *RemoveCallRecord:
		return p.ChannelID
	case RemoveCallRecord:
		return p.ChannelID
	case *UpsertProject:
		return p.ProjectID
	case UpsertProject:
		return p.ProjectID
	case *UpsertCalendarEvent:
		return p.EventID
	case UpsertCalendarEvent:
		return p.EventID
	case *string:
		return *p
	case string:
		return p
	}
	return ""
}

// ---------------------------------------------------------------------------
// Backfill request bodies (domain::models)
// ---------------------------------------------------------------------------

// DeletionFilter mirrors DeletionFilter (any/active/deleted).
type DeletionFilter string

// Deletion filter values (serde snake_case).
const (
	DeletionAny     DeletionFilter = "any"
	DeletionActive  DeletionFilter = "active"
	DeletionDeleted DeletionFilter = "deleted"
)

// onlyDeleted maps the filter to the nullable bool the queries take;
// "" (unset) is treated as "any" like the Rust serde default.
func (f DeletionFilter) onlyDeleted() *bool {
	switch f {
	case DeletionActive:
		b := false
		return &b
	case DeletionDeleted:
		b := true
		return &b
	}
	return nil
}

// CallBackfillRequest mirrors CallBackfillRequest.
type CallBackfillRequest struct {
	CallIDs       []string   `json:"call_ids"`
	StartedAfter  *time.Time `json:"started_after"`
	StartedBefore *time.Time `json:"started_before"`
	IndexOverride *string    `json:"index_override"`
}

// ChatBackfillRequest mirrors ChatBackfillRequest.
type ChatBackfillRequest struct {
	ChatIDs        []string       `json:"chat_ids"`
	UserIDs        []string       `json:"user_ids"`
	UpdatedAfter   *time.Time     `json:"updated_after"`
	UpdatedBefore  *time.Time     `json:"updated_before"`
	DeletionFilter DeletionFilter `json:"deletion_filter"`
	IndexOverride  *string        `json:"index_override"`
}

// ChannelBackfillRequest mirrors ChannelBackfillRequest.
type ChannelBackfillRequest struct {
	DeletionFilter DeletionFilter `json:"deletion_filter"`
	IndexOverride  *string        `json:"index_override"`
}

// DocumentBackfillRequest mirrors DocumentBackfillRequest.
type DocumentBackfillRequest struct {
	FileTypes      []string       `json:"file_types"`
	SubType        *string        `json:"sub_type"`
	UpdatedAfter   *time.Time     `json:"updated_after"`
	UpdatedBefore  *time.Time     `json:"updated_before"`
	DeletionFilter DeletionFilter `json:"deletion_filter"`
	IndexOverride  *string        `json:"index_override"`
}

// EmailBackfillRequest mirrors EmailBackfillRequest.
type EmailBackfillRequest struct {
	ThreadIDs     []string   `json:"thread_ids"`
	Since         *time.Time `json:"since"`
	IndexOverride *string    `json:"index_override"`
	BatchSize     *int       `json:"batch_size"`
}

// ProjectBackfillRequest mirrors ProjectBackfillRequest.
type ProjectBackfillRequest struct {
	UpdatedAfter  *time.Time `json:"updated_after"`
	UpdatedBefore *time.Time `json:"updated_before"`
	IndexOverride *string    `json:"index_override"`
}

// CalendarEventBackfillRequest mirrors CalendarEventBackfillRequest.
type CalendarEventBackfillRequest struct {
	UpdatedAfter  *time.Time `json:"updated_after"`
	UpdatedBefore *time.Time `json:"updated_before"`
	IndexOverride *string    `json:"index_override"`
}

// PropertiesBackfillRequest mirrors PropertiesBackfillRequest.
type PropertiesBackfillRequest struct {
	EntityType string `json:"entity_type"`
}

// BackfillReceipt is the terminal result of a drain.
type BackfillReceipt struct {
	Enqueued int `json:"enqueued"`
}
