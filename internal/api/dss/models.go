// Package dss ports services/document_storage_service plus the usable core of
// crates/documents, crates/projects and crates/entity_access to Go.
//
// See docs/GO_SELFHOST_PLAN.md — DynamoDB usage is folded into Postgres,
// presigned URLs go to MinIO via pkg/objectstore, and document lifecycle
// events publish Envelopes to the JetStream "macro.documents" stream
// (subject-sharded macro.documents.<id>).
package dss

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ---------- shared envelope ----------

// TypedSuccessResponse mirrors model::response::TypedSuccessResponse<T>:
// {"error": false, "data": ...} with camelCase data fields.
type TypedSuccessResponse[T any] struct {
	Error bool `json:"error"`
	Data  T    `json:"data"`
}

func okData[T any](data T) TypedSuccessResponse[T] {
	return TypedSuccessResponse[T]{Error: false, Data: data}
}

// GenericResponse mirrors model::response::GenericResponse.
type GenericResponse struct {
	Error   bool            `json:"error"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// ---------- owner ----------

// Owner mirrors model_owner::Owner serialized as its principal string
// ("macro|<email>" | "bot|<uuid>" | "<team uuid>").
type Owner string

// OwnerType mirrors model_owner::OwnerType (lowercase).
type OwnerType string

const (
	OwnerTypeUser OwnerType = "user"
	OwnerTypeBot  OwnerType = "bot"
	OwnerTypeTeam OwnerType = "team"
)

// Type infers the owner type from the principal string form.
func (o Owner) Type() OwnerType {
	s := string(o)
	switch {
	case len(s) >= 6 && s[:6] == "macro|":
		return OwnerTypeUser
	case len(s) >= 4 && s[:4] == "bot|":
		return OwnerTypeBot
	default:
		return OwnerTypeTeam
	}
}

// ---------- documents ----------

// DocumentMetadata mirrors model::document::DocumentMetadata (camelCase).
type DocumentMetadata struct {
	DocumentID            string          `json:"documentId"`
	DocumentVersionID     int64           `json:"documentVersionId"`
	Owner                 Owner           `json:"owner"`
	DocumentName          string          `json:"documentName"`
	FileType              *string         `json:"fileType,omitempty"`
	Sha                   *string         `json:"sha,omitempty"`
	ProjectID             *string         `json:"projectId,omitempty"`
	ProjectName           *string         `json:"projectName,omitempty"`
	BranchedFromID        *string         `json:"branchedFromId,omitempty"`
	BranchedFromVersionID *int64          `json:"branchedFromVersionId,omitempty"`
	DocumentFamilyID      *int64          `json:"documentFamilyId,omitempty"`
	DocumentBom           json.RawMessage `json:"documentBom,omitempty"`
	ModificationData      json.RawMessage `json:"modificationData,omitempty"`
	CreatedAt             *time.Time      `json:"createdAt"`
	UpdatedAt             *time.Time      `json:"updatedAt"`
	DeletedAt             *time.Time      `json:"deletedAt"`
	SubType               *string         `json:"subType,omitempty"`
}

// DocumentResponseMetadata mirrors model::document::DocumentResponseMetadata.
type DocumentResponseMetadata struct {
	DocumentID            string          `json:"documentId"`
	DocumentVersionID     int64           `json:"documentVersionId"`
	Owner                 Owner           `json:"owner"`
	DocumentName          string          `json:"documentName"`
	FileType              *string         `json:"fileType,omitempty"`
	Sha                   *string         `json:"sha,omitempty"`
	BranchedFromID        *string         `json:"branchedFromId,omitempty"`
	BranchedFromVersionID *int64          `json:"branchedFromVersionId,omitempty"`
	DocumentFamilyID      *int64          `json:"documentFamilyId,omitempty"`
	DocumentBom           json.RawMessage `json:"documentBom,omitempty"`
	ModificationData      json.RawMessage `json:"modificationData,omitempty"`
	CreatedAt             *time.Time      `json:"createdAt"`
	UpdatedAt             *time.Time      `json:"updatedAt"`
	SubType               *string         `json:"subType,omitempty"`
}

// DocumentResponse mirrors model::document::DocumentResponse.
type DocumentResponse struct {
	DocumentMetadata DocumentResponseMetadata `json:"documentMetadata"`
	PresignedURL     *string                  `json:"presignedUrl,omitempty"`
}

// CreateDocumentRequest mirrors model::document::CreateDocumentRequest.
type CreateDocumentRequest struct {
	ID                    *string    `json:"id"`
	Sha                   string     `json:"sha"`
	DocumentName          string     `json:"documentName"`
	FileType              *string    `json:"fileType"`
	MimeType              *string    `json:"mimeType"`
	DocumentFamilyID      *int64     `json:"documentFamilyId"`
	BranchedFromID        *string    `json:"branchedFromId"`
	BranchedFromVersionID *int64     `json:"branchedFromVersionId"`
	JobID                 *string    `json:"jobId"`
	ProjectID             *string    `json:"projectId"`
	TeamID                *string    `json:"teamId"`
	EmailAttachmentID     *string    `json:"emailAttachmentId"`
	CreatedAt             *time.Time `json:"createdAt"`
	IsTask                bool       `json:"isTask"`
	SkipHistory           bool       `json:"skipHistory"`
}

// CreateDocumentResponseData mirrors model::document::CreateDocumentResponseData
// (flattens DocumentResponse).
type CreateDocumentResponseData struct {
	DocumentMetadata DocumentResponseMetadata `json:"documentMetadata"`
	PresignedURL     *string                  `json:"presignedUrl,omitempty"`
	ContentType      string                   `json:"contentType"`
	FileType         *string                  `json:"fileType"`
}

// GetDocumentResponseData mirrors model::document::GetDocumentResponseData.
type GetDocumentResponseData struct {
	DocumentMetadata DocumentMetadata `json:"documentMetadata"`
	UserAccessLevel  string           `json:"userAccessLevel"`
	ViewLocation     *string          `json:"viewLocation"`
}

// GetDocumentListResult mirrors model::document::GetDocumentListResult.
type GetDocumentListResult struct {
	DocumentID            string     `json:"documentId"`
	DocumentVersionID     int64      `json:"documentVersionId"`
	DocumentName          string     `json:"documentName"`
	FileType              *string    `json:"fileType,omitempty"`
	BranchedFromID        *string    `json:"branchedFromId,omitempty"`
	BranchedFromVersionID *int64     `json:"branchedFromVersionId,omitempty"`
	DocumentFamilyID      *int64     `json:"documentFamilyId,omitempty"`
	CreatedAt             *time.Time `json:"createdAt"`
	UpdatedAt             *time.Time `json:"updatedAt"`
}

// UserDocumentsResponse mirrors the dss get_user_documents response data.
// Note: snake_case — the Rust struct has no rename_all.
type UserDocumentsResponse struct {
	Documents  []DocumentMetadata `json:"documents"`
	Total      int64              `json:"total"`
	NextOffset *int64             `json:"next_offset,omitempty"`
}

// ListDocumentsWithAccessRow mirrors model::document::list::DocumentListItem
// (snake_case — no rename_all on the Rust struct).
type ListDocumentsWithAccessRow struct {
	DocumentID   string     `json:"document_id"`
	DocumentName string     `json:"document_name"`
	Owner        Owner      `json:"owner"`
	FileType     *string    `json:"file_type"`
	ProjectID    *string    `json:"project_id"`
	CreatedAt    *time.Time `json:"created_at"`
	UpdatedAt    *time.Time `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at"`
	AccessLevel  string     `json:"access_level"`
}

// EditDocumentRequest mirrors crates/documents edit_document body
// (rename / move to project). Only fields the Go port applies are honored.
type EditDocumentRequest struct {
	DocumentName *string `json:"documentName"`
	ProjectID    *string `json:"projectId"`
}

// ---------- misc ----------

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func int8Ptr(i pgtype.Int8) *int64 {
	if !i.Valid {
		return nil
	}
	return &i.Int64
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	tt := t.Time
	return &tt
}

func bytesToRaw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}
