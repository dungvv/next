package dss

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/macro-inc/macro/pkg/events"
)

// Document lifecycle event types on the "macro.documents" JetStream stream
// (replacing the Kafka topic of the same name). The data payload keeps the
// Rust wire shape: {"event_id","schema_version","event_type","metadata"}.
const (
	DocEventCreated         = "document.created"
	DocEventUpdated         = "document.updated"
	DocEventDeleted         = "document.deleted"
	DocEventPurged          = "document.purged"
	DocEventCopied          = "document.copied"
	DocEventContentUploaded = "document.content_uploaded"

	docEventSchemaVersion = 1
)

// documentEventPayload mirrors macro_event_broker::Event<DocumentTopicEvent>
// — the Rust consumers (soup_realtime, search indexing) decode this exact
// shape.
type documentEventPayload struct {
	EventID       string          `json:"event_id"`
	SchemaVersion uint8           `json:"schema_version"`
	EventType     string          `json:"event_type"`
	Metadata      json.RawMessage `json:"metadata"`
}

// DocumentCreatedMetadata mirrors documents::domain::events::
// DocumentCreatedMetadata (snake_case serde fields).
type DocumentCreatedMetadata struct {
	DocumentID   string  `json:"document_id"`
	Owner        string  `json:"owner"`
	Actor        *string `json:"actor,omitempty"`
	OnBehalfOf   *string `json:"on_behalf_of,omitempty"`
	DocumentName string  `json:"document_name"`
	FileType     *string `json:"file_type"`
	ProjectID    *string `json:"project_id"`
	SubType      *string `json:"sub_type"`
	CreatedAt    *string `json:"created_at"`
}

// DocumentUpdatedMetadata mirrors DocumentUpdatedMetadata.
type DocumentUpdatedMetadata struct {
	DocumentID             string  `json:"document_id"`
	Owner                  string  `json:"owner"`
	ActorUserID            *string `json:"actor_user_id"`
	Actor                  *string `json:"actor,omitempty"`
	OnBehalfOf             *string `json:"on_behalf_of,omitempty"`
	DocumentName           *string `json:"document_name"`
	PreviousProjectID      *string `json:"previous_project_id"`
	ProjectID              *string `json:"project_id"`
	FileType               any     `json:"file_type"`
	SharePermissionUpdated bool    `json:"share_permission_updated"`
}

// DocumentDeletedMetadata mirrors DocumentDeletedMetadata.
type DocumentDeletedMetadata struct {
	DocumentID  string  `json:"document_id"`
	ActorUserID *string `json:"actor_user_id"`
	Actor       *string `json:"actor,omitempty"`
	OnBehalfOf  *string `json:"on_behalf_of,omitempty"`
	ProjectID   *string `json:"project_id"`
}

// DocumentPurgedMetadata mirrors DocumentPurgedMetadata.
type DocumentPurgedMetadata struct {
	DocumentID string `json:"document_id"`
}

// DocumentContentUploadedMetadata mirrors DocumentContentUploadedMetadata.
type DocumentContentUploadedMetadata struct {
	DocumentID        string  `json:"document_id"`
	Owner             string  `json:"owner"`
	FileType          string  `json:"file_type"`
	DocumentVersionID *string `json:"document_version_id"`
}

// publishDocumentEvent wraps the Rust payload in the shared events.Envelope
// and publishes to macro.documents.<document_id>. Best-effort: failures are
// logged, never returned (matches publish_document_event which spawns and
// ignores errors).
func (s *Service) publishDocumentEvent(documentID, eventType string, metadata any) {
	if s.js == nil {
		return
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		slog.Error("dss: marshal document event metadata", "err", err)
		return
	}
	payload := documentEventPayload{
		EventID:       uuid.NewString(),
		SchemaVersion: docEventSchemaVersion,
		EventType:     eventType,
		Metadata:      raw,
	}
	env, err := events.New(eventType, "dss", documentID, docEventSchemaVersion, payload)
	if err != nil {
		slog.Error("dss: build event envelope", "err", err)
		return
	}
	body, err := json.Marshal(env)
	if err != nil {
		slog.Error("dss: marshal event envelope", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.js.Publish(ctx, events.Shard(events.StreamDocuments, documentID), body); err != nil {
		slog.Error("dss: publish document event", "type", eventType, "doc", documentID, "err", err)
	}
}
