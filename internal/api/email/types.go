package email

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// API models mirror the Rust service's serde shapes (snake_case).

// ContactInfo is the {email,name,photo_url} triple used across messages/drafts.
type ContactInfo struct {
	Email    string  `json:"email"`
	Name     *string `json:"name"`
	PhotoURL *string `json:"photo_url"`
}

// AttachmentDraft mirrors api::AttachmentDraft.
type AttachmentDraft struct {
	ID          uuid.UUID `json:"id"`
	DraftID     uuid.UUID `json:"draft_id"`
	FileName    string    `json:"file_name"`
	ContentType string    `json:"content_type"`
	Sha         string    `json:"sha"`
	Size        int32     `json:"size"`
	S3Key       string    `json:"s3_key"`
}

// AttachmentForwarded mirrors api::AttachmentForwarded.
type AttachmentForwarded struct {
	AttachmentID         uuid.UUID `json:"attachment_id"`
	DraftID              uuid.UUID `json:"draft_id"`
	ProviderAttachmentID *string   `json:"provider_attachment_id"`
	MessageProviderID    string    `json:"message_provider_id"`
	Filename             *string   `json:"filename"`
	MimeType             *string   `json:"mime_type"`
	SizeBytes            *int64    `json:"size_bytes"`
}

// MessageAttachment mirrors api::Attachment / service::attachment::Attachment.
type MessageAttachment struct {
	DBID       uuid.UUID  `json:"db_id"`
	ProviderID *string    `json:"provider_id"`
	DataURL    *string    `json:"data_url,omitempty"`
	Filename   *string    `json:"filename"`
	MimeType   *string    `json:"mime_type"`
	SizeBytes  *int64     `json:"size_bytes"`
	SfsID      *uuid.UUID `json:"sfs_id"`
	ContentID  *string    `json:"content_id"`
}

// MessageLabel mirrors ApiMessageLabel.
type MessageLabel struct {
	ID                    *uuid.UUID `json:"id"`
	LinkID                uuid.UUID  `json:"link_id"`
	ProviderLabelID       string     `json:"provider_label_id"`
	Name                  *string    `json:"name"`
	CreatedAt             time.Time  `json:"created_at"`
	MessageListVisibility *string    `json:"message_list_visibility"`
	LabelListVisibility   *string    `json:"label_list_visibility"`
	Type                  *string    `json:"type_"`
}

// Message mirrors ApiMessage.
type Message struct {
	DBID                 uuid.UUID             `json:"db_id"`
	ProviderID           *string               `json:"provider_id"`
	ThreadDBID           uuid.UUID             `json:"thread_db_id"`
	ProviderThreadID     *string               `json:"provider_thread_id"`
	ReplyingToID         *uuid.UUID            `json:"replying_to_id"`
	GlobalID             *string               `json:"global_id"`
	LinkID               uuid.UUID             `json:"link_id"`
	Subject              *string               `json:"subject"`
	Snippet              *string               `json:"snippet"`
	ProviderHistoryID    *string               `json:"provider_history_id"`
	SentAt               *time.Time            `json:"sent_at"`
	InternalDateTs       *time.Time            `json:"internal_date_ts"`
	SizeEstimate         *int64                `json:"size_estimate"`
	IsRead               bool                  `json:"is_read"`
	IsStarred            bool                  `json:"is_starred"`
	IsSent               bool                  `json:"is_sent"`
	IsDraft              bool                  `json:"is_draft"`
	HasAttachments       bool                  `json:"has_attachments"`
	ScheduledSendTime    *time.Time            `json:"scheduled_send_time,omitempty"`
	From                 *ContactInfo          `json:"from"`
	To                   []ContactInfo         `json:"to"`
	Cc                   []ContactInfo         `json:"cc"`
	Bcc                  []ContactInfo         `json:"bcc"`
	Labels               []MessageLabel        `json:"labels"`
	BodyText             *string               `json:"body_text"`
	BodyHTMLSanitized    *string               `json:"body_html_sanitized"`
	BodyMacro            *string               `json:"body_macro"`
	BodyReplyless        *string               `json:"body_replyless"`
	Attachments          []MessageAttachment   `json:"attachments"`
	AttachmentsDraft     []AttachmentDraft     `json:"attachments_draft"`
	AttachmentsForwarded []AttachmentForwarded `json:"attachments_forwarded"`
	HeadersJSON          json.RawMessage       `json:"headers_json"`
	CreatedAt            time.Time             `json:"created_at"`
	UpdatedAt            time.Time             `json:"updated_at"`
}

// Thread mirrors ApiThread.
type Thread struct {
	DBID                    uuid.UUID  `json:"db_id"`
	ProviderID              *string    `json:"provider_id"`
	LinkID                  uuid.UUID  `json:"link_id"`
	InboxVisible            bool       `json:"inbox_visible"`
	IsRead                  bool       `json:"is_read"`
	AccessLevel             string     `json:"access_level"`
	LatestInboundMessageTs  *time.Time `json:"latest_inbound_message_ts"`
	LatestOutboundMessageTs *time.Time `json:"latest_outbound_message_ts"`
	LatestNonSpamMessageTs  *time.Time `json:"latest_non_spam_message_ts"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	ProjectID               *string    `json:"project_id"`
	Messages                []Message  `json:"messages"`
}

type GetThreadResponse struct {
	Thread Thread `json:"thread"`
}

// --- drafts / send -----------------------------------------------------------

// DraftContactInfo mirrors ApiDraftContactInfo.
type DraftContactInfo struct {
	Email    string  `json:"email"`
	Name     *string `json:"name"`
	PhotoURL *string `json:"photo_url"`
}

// DraftInput mirrors ApiDraftInput (the old MessageToSend shape).
type DraftInput struct {
	DBID             *uuid.UUID         `json:"db_id"`
	ProviderID       *string            `json:"provider_id"`
	ReplyingToID     *uuid.UUID         `json:"replying_to_id"`
	ProviderThreadID *string            `json:"provider_thread_id"`
	ThreadDBID       *uuid.UUID         `json:"thread_db_id"`
	Subject          string             `json:"subject"`
	To               []DraftContactInfo `json:"to"`
	Cc               []DraftContactInfo `json:"cc"`
	Bcc              []DraftContactInfo `json:"bcc"`
	BodyText         *string            `json:"body_text"`
	BodyHTML         *string            `json:"body_html"`
	BodyMacro        *string            `json:"body_macro"`
	HeadersJSON      json.RawMessage    `json:"headers_json"`
	IncludeSignature *bool              `json:"include_signature"`
}

// DraftOutput mirrors ApiDraftOutput.
type DraftOutput struct {
	DBID             *uuid.UUID         `json:"db_id"`
	ProviderID       *string            `json:"provider_id"`
	ReplyingToID     *uuid.UUID         `json:"replying_to_id"`
	ProviderThreadID *string            `json:"provider_thread_id"`
	ThreadDBID       *uuid.UUID         `json:"thread_db_id"`
	LinkID           uuid.UUID          `json:"link_id"`
	Subject          string             `json:"subject"`
	To               []DraftContactInfo `json:"to"`
	Cc               []DraftContactInfo `json:"cc"`
	Bcc              []DraftContactInfo `json:"bcc"`
	BodyText         *string            `json:"body_text"`
	BodyHTML         *string            `json:"body_html"`
	BodyMacro        *string            `json:"body_macro"`
	HeadersJSON      json.RawMessage    `json:"headers_json"`
	SendTime         *time.Time         `json:"send_time"`
}

type CreateDraftRequest struct {
	Draft    DraftInput `json:"draft"`
	SendTime *time.Time `json:"send_time"`
}

type CreateDraftResponse struct {
	Draft DraftOutput `json:"draft"`
}

type SendMessageRequest struct {
	Message DraftInput `json:"message"`
}

type SendMessageResponse struct {
	Message DraftOutput `json:"message"`
}

// --- labels ------------------------------------------------------------------

type Label struct {
	ID                    uuid.UUID `json:"id"`
	LinkID                uuid.UUID `json:"link_id"`
	ProviderLabelID       string    `json:"provider_label_id"`
	Name                  string    `json:"name"`
	CreatedAt             time.Time `json:"created_at"`
	MessageListVisibility string    `json:"message_list_visibility"`
	LabelListVisibility   string    `json:"label_list_visibility"`
	Type                  string    `json:"type_"`
}

type ListLabelsResponse struct {
	Labels []Label `json:"labels"`
}

type CreateLabelRequest struct {
	Name string `json:"name"`
}

type CreateLabelResponse struct {
	Label Label `json:"label"`
}

type UpdateLabelBatchRequest struct {
	MessageIDs []uuid.UUID `json:"message_ids"`
	LabelID    uuid.UUID   `json:"label_id"`
	Value      bool        `json:"value"`
}

type UpdateLabelBatchResponse struct {
	SuccessfulIDs []uuid.UUID `json:"successful_ids"`
	FailedIDs     []uuid.UUID `json:"failed_ids"`
	MissingIDs    []uuid.UUID `json:"missing_ids"`
}

type UpdateThreadLabelRequest struct {
	LabelID uuid.UUID `json:"label_id"`
	Value   bool      `json:"value"`
}

type UpdateThreadLabelsResponse struct {
	SuccessfulIDs []uuid.UUID `json:"successful_ids"`
	FailedIDs     []uuid.UUID `json:"failed_ids"`
}

// --- threads -----------------------------------------------------------------

type ArchiveThreadRequest struct {
	Value bool `json:"value"`
}

// --- previews ----------------------------------------------------------------

// ThreadPreview mirrors ApiThreadPreviewCursor (flattened thread + lists).
type ThreadPreview struct {
	ID             uuid.UUID           `json:"id"`
	ProviderID     *string             `json:"provider_id,omitempty"`
	OwnerID        string              `json:"owner_id"`
	InboxVisible   bool                `json:"inbox_visible"`
	IsRead         bool                `json:"is_read"`
	IsDraft        bool                `json:"is_draft"`
	IsImportant    bool                `json:"is_important"`
	Name           *string             `json:"name"`
	Snippet        *string             `json:"snippet"`
	SenderEmail    *string             `json:"sender_email"`
	SenderName     *string             `json:"sender_name"`
	SenderPhotoURL *string             `json:"sender_photo_url"`
	SortTs         time.Time           `json:"sort_ts"`
	ViewedAt       *time.Time          `json:"viewed_at"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	ProjectID      *string             `json:"project_id"`
	LinkID         uuid.UUID           `json:"link_id"`
	Attachments    []MessageAttachment `json:"attachments"`
	Participants   []PreviewContact    `json:"participants"`
	Labels         []Label             `json:"labels"`
	FrecencyScore  *float64            `json:"frecency_score"`
}

// PreviewContact mirrors ApiContact.
type PreviewContact struct {
	ID           uuid.UUID `json:"id"`
	LinkID       uuid.UUID `json:"link_id"`
	Name         *string   `json:"name"`
	EmailAddress *string   `json:"email_address"`
	SfsPhotoURL  *string   `json:"sfs_photo_url"`
}

// PaginatedThreadCursor mirrors ApiPaginatedThreadCursor.
type PaginatedThreadCursor struct {
	Items      []ThreadPreview `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

// --- links -------------------------------------------------------------------

// Link mirrors api::link::Link.
type Link struct {
	ID                      uuid.UUID `json:"id"`
	MacroID                 string    `json:"macro_id"`
	FusionauthUserID        string    `json:"fusionauth_user_id"`
	EmailAddress            string    `json:"email_address"`
	PhotoURL                *string   `json:"photo_url"`
	Provider                string    `json:"provider"`
	IsSyncActive            bool      `json:"is_sync_active"`
	SyncStatus              string    `json:"sync_status"`
	NeedsReauth             bool      `json:"needs_reauth"`
	NeedsCalendarPermission bool      `json:"needs_calendar_permission"`
	CalendarDisabled        bool      `json:"calendar_disabled"`
	HasCalendarData         bool      `json:"has_calendar_data"`
	Settings                Settings  `json:"settings"`
	IsPrimary               bool      `json:"is_primary"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

type ListLinksResponse struct {
	Links []Link `json:"links"`
}

type ResyncResponse struct {
	Success       bool       `json:"success"`
	Message       *string    `json:"message,omitempty"`
	BackfillJobID *uuid.UUID `json:"backfill_job_id,omitempty"`
}

// --- contacts ----------------------------------------------------------------

// ContactInfoWithInteraction mirrors the Rust per-link contacts map value.
type ContactInfoWithInteraction struct {
	Email           string     `json:"email"`
	EmailAddress    string     `json:"email_address"`
	Name            *string    `json:"name"`
	PhotoURL        *string    `json:"photo_url"`
	LastInteraction *time.Time `json:"last_interaction"`
}

type ListContactsResponse struct {
	Contacts map[string][]ContactInfoWithInteraction `json:"contacts"`
}

type BlockSenderRequest struct {
	EmailAddress string `json:"email_address"`
}

type UnblockSenderRequest struct {
	EmailAddress string `json:"email_address"`
}

type ListBlockedResponse struct {
	BlockedEmails []string `json:"blocked_emails"`
}

// --- settings ----------------------------------------------------------------

type Settings struct {
	SignatureOnRepliesForwards *bool   `json:"signature_on_replies_forwards"`
	Signature                  *string `json:"signature"`
}

type PatchSettingsRequest struct {
	Settings Settings `json:"settings"`
}

type PatchSettingsResponse struct {
	Settings Settings `json:"settings"`
}

// --- backfill ----------------------------------------------------------------

type BackfillJob struct {
	ID                    uuid.UUID  `json:"id"`
	LinkID                *uuid.UUID `json:"link_id"`
	FusionauthUserID      string     `json:"fusionauth_user_id"`
	ThreadsRequestedLimit *int32     `json:"threads_requested_limit"`
	TotalThreads          int32      `json:"total_threads"`
	Status                string     `json:"status"`
	ThreadsRetrievedCount int32      `json:"threads_retrieved_count"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

type GetBackfillJobResponse struct {
	Job *BackfillJob `json:"job"`
}

type GetActiveBackfillJobResponse struct {
	Job *BackfillJob `json:"job"`
}

type ListBackfillJobsResponse struct {
	Jobs []BackfillJob `json:"jobs"`
}

type CancelBackfillParams struct {
	JobID uuid.UUID `json:"job_id"`
}

type CreateBackfillRequest struct {
	ThreadsRequestedLimit *int32 `json:"threads_requested_limit"`
}

// --- attachments ---------------------------------------------------------------

type GetAttachmentResponse struct {
	Attachment MessageAttachment `json:"attachment"`
}

type GetDocumentIDResponse struct {
	DocumentID *string `json:"document_id"`
}

type AddDraftAttachmentRequest struct {
	FileName string `json:"file_name"`
	Sha      string `json:"sha"`
	Size     int32  `json:"size"`
}

type AddDraftAttachmentResponse struct {
	AttachmentID uuid.UUID `json:"attachment_id"`
	UploadURL    string    `json:"upload_url"`
	ContentType  string    `json:"content_type"`
}

type AddForwardedAttachmentRequest struct {
	AttachmentID uuid.UUID `json:"attachment_id"`
}

// --- scheduled drafts ----------------------------------------------------------

type UpsertScheduledRequest struct {
	SendTime time.Time `json:"send_time"`
}

type UpsertScheduledResponse struct {
	MessageID uuid.UUID `json:"message_id"`
	SendTime  time.Time `json:"send_time"`
}

type GetScheduledResponse struct {
	Messages []Message `json:"messages"`
}

// --- init / webhook ------------------------------------------------------------

type InitResponse struct {
	LinkID        uuid.UUID  `json:"link_id"`
	BackfillJobID *uuid.UUID `json:"backfill_job_id,omitempty"`
}

// pubsubEnvelope mirrors the Gmail push payload POSTed by Google Pub/Sub.
type pubsubEnvelope struct {
	Message struct {
		Data        []byte `json:"data"` // base64 {"emailAddress","historyId"}
		MessageID   string `json:"messageId"`
		PublishTime string `json:"publishTime"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

type gmailPushData struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    int64  `json:"historyId"`
}
