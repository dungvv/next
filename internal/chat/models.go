package chat

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Wire types below mirror the serde shapes of crates/channels domain models
// (snake_case JSON unless noted).

// ChannelType mirrors comms_channel_type.
type ChannelType string

const (
	ChannelPublic  ChannelType = "public"
	ChannelPrivate ChannelType = "private"
	ChannelDM      ChannelType = "direct_message"
	ChannelTeam    ChannelType = "team"
)

// ParticipantRole mirrors comms_participant_role.
type ParticipantRole string

const (
	RoleOwner  ParticipantRole = "owner"
	RoleAdmin  ParticipantRole = "admin"
	RoleMember ParticipantRole = "member"
)

// SimpleMention mirrors comms_db_client::model::SimpleMention.
type SimpleMention struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
}

// NewChannelAttachment mirrors channels::models::NewChannelAttachment.
type NewChannelAttachment struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Width      *int32 `json:"width,omitempty"`
	Height     *int32 `json:"height,omitempty"`
}

type CreateChannelRequest struct {
	Name         *string     `json:"name"`
	ChannelType  ChannelType `json:"channel_type"`
	TeamID       *uuid.UUID  `json:"team_id"`
	AutoJoinTeam bool        `json:"auto_join_team"`
	Participants []string    `json:"participants"`
}

type CreateChannelResponse struct {
	ID string `json:"id"`
}

type GetOrCreateDmRequest struct {
	RecipientID string `json:"recipient_id"`
}

type GetOrCreatePrivateRequest struct {
	Recipients []string `json:"recipients"`
}

// GetOrCreateChannelResponse: action is "get" or "create".
type GetOrCreateChannelResponse struct {
	ChannelID string `json:"channel_id"`
	Action    string `json:"action"`
}

type PatchChannelRequest struct {
	ChannelName          *string `json:"channel_name"`
	ConvertToTeamChannel *bool   `json:"convert_to_team_channel"`
	AutoJoinTeam         *bool   `json:"auto_join_team"`
}

type ChannelJoinCodeResponse struct {
	JoinCode uuid.UUID `json:"join_code"`
}

type PostMessageRequest struct {
	Content     string                 `json:"content"`
	Mentions    []SimpleMention        `json:"mentions"`
	ThreadID    *uuid.UUID             `json:"thread_id"`
	Attachments []NewChannelAttachment `json:"attachments"`
	Nonce       *string                `json:"nonce"`
}

type PostMessageResponse struct {
	ID    string  `json:"id"`
	Nonce *string `json:"nonce"`
}

type PatchMessageRequest struct {
	Content               *string                 `json:"content"`
	Mentions              *[]SimpleMention        `json:"mentions"`
	AttachmentIDsToDelete *[]string               `json:"attachment_ids_to_delete"`
	AttachmentsToAdd      *[]NewChannelAttachment `json:"attachments_to_add"`
	Nonce                 *string                 `json:"nonce"`
}

type PostReactionRequest struct {
	Emoji     string  `json:"emoji"`
	MessageID string  `json:"message_id"`
	Action    string  `json:"action"` // "add" | "remove"
	Nonce     *string `json:"nonce"`
}

type PostTypingRequest struct {
	Action   string  `json:"action"` // "start" | "stop"
	ThreadID *string `json:"thread_id"`
	Nonce    *string `json:"nonce"`
}

type AddParticipantsRequest struct {
	Participants []string `json:"participants"`
}

type RemoveParticipantsRequest struct {
	Participants []string `json:"participants"`
}

type PostActivityRequest struct {
	ChannelID    uuid.UUID `json:"channel_id"`
	ActivityType string    `json:"activity_type"` // "view" | "interact"
}

// UserActivityRow is a comms_activity row for GET /channels/activity.
type UserActivityRow struct {
	ChannelID    uuid.UUID  `json:"channel_id"`
	ViewedAt     *time.Time `json:"viewed_at"`
	InteractedAt *time.Time `json:"interacted_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ChannelParticipant mirrors channels::models::ChannelParticipant.
type ChannelParticipant struct {
	ChannelID uuid.UUID       `json:"channel_id"`
	UserID    string          `json:"user_id"`
	Role      ParticipantRole `json:"role"`
	JoinedAt  time.Time       `json:"joined_at"`
	LeftAt    *time.Time      `json:"left_at"`
}

// CountedReaction mirrors channels::models::CountedReaction.
type CountedReaction struct {
	Emoji string   `json:"emoji"`
	Users []string `json:"users"`
}

// MessageAttachment mirrors channels::models::MessageAttachment.
type MessageAttachment struct {
	ID         uuid.UUID `json:"id"`
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	Width      *int32    `json:"width,omitempty"`
	Height     *int32    `json:"height,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ThreadReply mirrors channels::models::ThreadReply.
type ThreadReply struct {
	ID          uuid.UUID           `json:"id"`
	SenderID    string              `json:"sender_id"`
	TriggeredBy *string             `json:"triggered_by,omitempty"`
	Content     string              `json:"content"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
	EditedAt    *time.Time          `json:"edited_at"`
	Reactions   []CountedReaction   `json:"reactions"`
	Attachments []MessageAttachment `json:"attachments"`
}

// ThreadInfo mirrors channels::models::ThreadInfo.
type ThreadInfo struct {
	ReplyCount    int64         `json:"reply_count"`
	LatestReplyAt *time.Time    `json:"latest_reply_at"`
	Preview       []ThreadReply `json:"preview"`
}

// ChannelMessage mirrors channels::models::ChannelMessage (REST shape).
type ChannelMessage struct {
	ID          uuid.UUID           `json:"id"`
	ChannelID   uuid.UUID           `json:"channel_id"`
	SenderID    string              `json:"sender_id"`
	TriggeredBy *string             `json:"triggered_by"`
	Content     string              `json:"content"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
	EditedAt    *time.Time          `json:"edited_at"`
	DeletedAt   *time.Time          `json:"deleted_at"`
	Thread      ThreadInfo          `json:"thread"`
	Reactions   []CountedReaction   `json:"reactions"`
	Attachments []MessageAttachment `json:"attachments"`
}

// MutatedMessage mirrors channels::models::MutatedMessage — the realtime
// payload for comms_message frames.
type MutatedMessage struct {
	ID          uuid.UUID  `json:"id"`
	ChannelID   uuid.UUID  `json:"channel_id"`
	ThreadID    *uuid.UUID `json:"thread_id"`
	SenderID    string     `json:"sender_id"`
	TriggeredBy *string    `json:"triggered_by"`
	Content     string     `json:"content"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	EditedAt    *time.Time `json:"edited_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
}

// Activity mirrors channels::models::Activity.
type Activity struct {
	ID           uuid.UUID  `json:"id"`
	UserID       string     `json:"user_id"`
	ChannelID    uuid.UUID  `json:"channel_id"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	ViewedAt     *time.Time `json:"viewed_at"`
	InteractedAt *time.Time `json:"interacted_at"`
}

// EntityMention mirrors channels::models::EntityMention.
type EntityMention struct {
	ID               uuid.UUID `json:"id"`
	SourceEntityType string    `json:"source_entity_type"`
	SourceEntityID   string    `json:"source_entity_id"`
	EntityType       string    `json:"entity_type"`
	EntityID         string    `json:"entity_id"`
	UserID           *string   `json:"user_id"`
	CreatedAt        time.Time `json:"created_at"`
}

type CreateEntityMentionRequest struct {
	SourceEntityType string `json:"source_entity_type"`
	SourceEntityID   string `json:"source_entity_id"`
	EntityType       string `json:"entity_type"`
	EntityID         string `json:"entity_id"`
}

type DeleteEntityMentionResponse struct {
	Deleted bool `json:"deleted"`
}

// ChannelAttachment mirrors channels::models::ChannelAttachment.
type ChannelAttachment struct {
	ID         uuid.UUID `json:"id"`
	ChannelID  uuid.UUID `json:"channel_id"`
	MessageID  uuid.UUID `json:"message_id"`
	SenderID   string    `json:"sender_id"`
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	Width      *int32    `json:"width,omitempty"`
	Height     *int32    `json:"height,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ChannelPreview mirrors the tagged ChannelPreview enum:
// {"type": "access"|"no_access"|"does_not_exist", ...}.
type ChannelPreview map[string]any

type GetBatchChannelPreviewRequest struct {
	ChannelIDs []string `json:"channel_ids"`
}

type GetBatchChannelPreviewResponse struct {
	Previews []ChannelPreview `json:"previews"`
}

// RecentChannelMessage mirrors channels::models::RecentChannelMessage.
type RecentChannelMessage struct {
	MessageID uuid.UUID  `json:"message_id"`
	ThreadID  *uuid.UUID `json:"thread_id"`
	SenderID  string     `json:"sender_id"`
	Content   string     `json:"content"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at"`
	Mentions  []string   `json:"mentions"`
}

// ChannelListItem is the /comms/channels response element — channel row plus
// participants, latest messages, and the caller's activity.
type ChannelListItem struct {
	ID                     uuid.UUID             `json:"id"`
	Name                   *string               `json:"name"`
	ChannelType            ChannelType           `json:"channel_type"`
	OrgID                  *int64                `json:"org_id"`
	TeamID                 *uuid.UUID            `json:"team_id"`
	AutoJoinTeam           bool                  `json:"auto_join_team"`
	CreatedAt              time.Time             `json:"created_at"`
	UpdatedAt              time.Time             `json:"updated_at"`
	OwnerID                string                `json:"owner_id"`
	Participants           []ChannelParticipant  `json:"participants"`
	IsParticipant          bool                  `json:"is_participant"`
	LatestMessage          *RecentChannelMessage `json:"latest_message"`
	LatestNonThreadMessage *RecentChannelMessage `json:"latest_non_thread_message"`
	ViewedAt               *time.Time            `json:"viewed_at"`
	InteractedAt           *time.Time            `json:"interacted_at"`
	FrecencyScore          *float64              `json:"frecency_score"`
}

// ChannelListPage mirrors ApiChannelListPage ({items, next_cursor}).
type ChannelListPage struct {
	Items      []ChannelListItem `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// GetChannelResponse is the Rust channel-with-participants payload returned
// by GET /channels/{id} (ChannelWithParticipants serialized shape).
type GetChannelResponse struct {
	ID            uuid.UUID            `json:"id"`
	Name          *string              `json:"name"`
	ChannelType   ChannelType          `json:"channel_type"`
	OrgID         *int64               `json:"org_id"`
	TeamID        *uuid.UUID           `json:"team_id"`
	AutoJoinTeam  bool                 `json:"auto_join_team"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
	OwnerID       string               `json:"owner_id"`
	JoinCode      *uuid.UUID           `json:"join_code,omitempty"`
	Participants  []ChannelParticipant `json:"participants"`
	IsParticipant bool                 `json:"is_participant"`
}

// AttachmentReference mirrors the tagged AttachmentEntityReference enum.
type AttachmentReference map[string]any

// errorBody is the shared {message} error shape the frontend consumes.
type errorBody struct {
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorBody{Message: msg})
}
