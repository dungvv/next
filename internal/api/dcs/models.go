// Wire models for the document cognition service chat API. Field names mirror
// the Rust crates (model::chat, model_entity::Entity, chat::domain::models)
// so the existing frontend contract is preserved.
package dcs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// EntityType mirrors model_entity::EntityType (serde snake_case).
type EntityType string

const (
	EntityTypeUser            EntityType = "user"
	EntityTypeChat            EntityType = "chat"
	EntityTypeChannel         EntityType = "channel"
	EntityTypeChannelMessage  EntityType = "channel_message"
	EntityTypeDocument        EntityType = "document"
	EntityTypeProject         EntityType = "project"
	EntityTypeEmailThread     EntityType = "email_thread"
	EntityTypeCalendarEvent   EntityType = "calendar_event"
	EntityTypeTeam            EntityType = "team"
	EntityTypeCall            EntityType = "call"
	EntityTypeForeignEntity   EntityType = "foreign_entity"
	EntityTypeStaticFile      EntityType = "static_file"
	EntityTypeCRMCompany      EntityType = "crm_company"
	EntityTypeCRMContact      EntityType = "crm_contact"
	EntityTypeReminder        EntityType = "reminder"
	EntityTypeSkill           EntityType = "skill"
	EntityTypeAgentSession    EntityType = "agent_session"
	EntityTypeScheduledAction EntityType = "scheduled_action"
	EntityTypeInitiative      EntityType = "initiative"
)

// Entity mirrors model_entity::Entity (snake_case fields).
type Entity struct {
	EntityType EntityType `json:"entity_type"`
	EntityID   string     `json:"entity_id"`
}

// Chat message roles (agent::types::Role, lowercase strings).
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// ChatMessageContent mirrors agent::types::ChatMessageContent: untagged JSON,
// either a plain string ("text" content) or an array of assistant message
// parts. It round-trips raw JSON so persisted content is preserved verbatim.
type MessageContent struct {
	raw json.RawMessage
}

// TextContent builds a plain-text message content value.
func TextContent(text string) MessageContent {
	raw, _ := json.Marshal(text)
	return MessageContent{raw: raw}
}

// PartsContent builds an assistant message content value from parts.
func PartsContent(parts []MessagePart) MessageContent {
	raw, _ := json.Marshal(parts)
	return MessageContent{raw: raw}
}

// Raw returns the raw JSON representation (a string or an array of parts).
func (c MessageContent) Raw() json.RawMessage { return c.raw }

// Text returns the string content when the message is a plain text message.
func (c MessageContent) Text() (string, bool) {
	var s string
	if err := json.Unmarshal(c.raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// Parts returns the structured parts when the message is an assistant-parts
// message.
func (c MessageContent) Parts() ([]MessagePart, bool) {
	var parts []MessagePart
	if err := json.Unmarshal(c.raw, &parts); err != nil {
		return nil, false
	}
	return parts, true
}

// MessageText returns the concatenated text of the message, whether it is a
// plain string or an assistant parts array (mirrors message_text()).
func (c MessageContent) MessageText() string {
	if s, ok := c.Text(); ok {
		return s
	}
	if parts, ok := c.Parts(); ok {
		var b bytes.Buffer
		for _, p := range parts {
			if p.Type == PartText {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// MarshalJSON passthrough of the raw content.
func (c MessageContent) MarshalJSON() ([]byte, error) {
	if len(c.raw) == 0 {
		return []byte(`""`), nil
	}
	return c.raw, nil
}

// UnmarshalJSON stores the raw content verbatim.
func (c *MessageContent) UnmarshalJSON(data []byte) error {
	c.raw = append(c.raw[:0], data...)
	return nil
}

// Assistant message part types (agent::types::AssistantMessagePart, tagged
// camelCase `type` discriminator).
const (
	PartText                 = "text"
	PartToolCall             = "toolCall"
	PartMCPToolCall          = "mcpToolCall"
	PartToolCallResponseJSON = "toolCallResponseJson"
	PartToolCallErr          = "toolCallErr"
	PartThinking             = "thinking"
)

// MessagePart mirrors agent::types::AssistantMessagePart. The enum is
// `#[serde(tag="type", rename_all="camelCase")]`: variant names are camelCase
// ("toolCall", …) while the variant fields keep their Rust names — `id`,
// `name`, `json`, `description`, `service`, `display_name`, `text`,
// `thinking` — because serde ≥1.0.190 only applies rename_all to the variants.
// Only the fields of the matching variant are serialized.
type MessagePart struct {
	Type string `json:"-"`
	// text
	Text string `json:"-"`
	// toolCall / mcpToolCall / toolCallResponseJson / toolCallErr
	ID string `json:"-"`
	// toolCall / mcpToolCall / toolCallResponseJson / toolCallErr
	Name string `json:"-"`
	// toolCall / mcpToolCall / toolCallResponseJson
	JSON json.RawMessage `json:"-"`
	// mcpToolCall
	Service     string `json:"-"`
	DisplayName string `json:"-"`
	// toolCallErr
	Description string `json:"-"`
	// thinking
	Thinking string `json:"-"`
}

// MarshalJSON emits exactly the fields of the matching Rust variant.
func (p MessagePart) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": p.Type}
	switch p.Type {
	case PartText:
		m["text"] = p.Text
	case PartToolCall:
		m["name"] = p.Name
		if len(p.JSON) > 0 {
			m["json"] = json.RawMessage(p.JSON)
		} else {
			m["json"] = json.RawMessage(`{}`)
		}
		m["id"] = p.ID
	case PartMCPToolCall:
		m["name"] = p.Name
		m["service"] = p.Service
		m["display_name"] = p.DisplayName
		if len(p.JSON) > 0 {
			m["json"] = json.RawMessage(p.JSON)
		} else {
			m["json"] = json.RawMessage(`{}`)
		}
		m["id"] = p.ID
	case PartToolCallResponseJSON:
		m["name"] = p.Name
		if len(p.JSON) > 0 {
			m["json"] = json.RawMessage(p.JSON)
		} else {
			m["json"] = json.RawMessage(`null`)
		}
		m["id"] = p.ID
	case PartToolCallErr:
		m["name"] = p.Name
		m["description"] = p.Description
		m["id"] = p.ID
	case PartThinking:
		m["thinking"] = p.Thinking
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads a tagged part.
func (p *MessagePart) UnmarshalJSON(data []byte) error {
	var aux struct {
		Type        string          `json:"type"`
		Text        string          `json:"text"`
		ID          string          `json:"id"`
		Name        string          `json:"name"`
		JSON        json.RawMessage `json:"json"`
		Service     string          `json:"service"`
		DisplayName string          `json:"display_name"`
		Description string          `json:"description"`
		Thinking    string          `json:"thinking"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*p = MessagePart{
		Type:        aux.Type,
		Text:        aux.Text,
		ID:          aux.ID,
		Name:        aux.Name,
		JSON:        aux.JSON,
		Service:     aux.Service,
		DisplayName: aux.DisplayName,
		Description: aux.Description,
		Thinking:    aux.Thinking,
	}
	return nil
}

// TextPart builds a {"type":"text"} part.
func TextPart(text string) MessagePart { return MessagePart{Type: PartText, Text: text} }

// ChatMessage mirrors model::chat::ChatMessageWithAttachments (camelCase).
type ChatMessage struct {
	ID          string         `json:"id"`
	Content     MessageContent `json:"content"`
	Role        string         `json:"role"`
	Attachments []Entity       `json:"attachments"`
}

// Chat mirrors model::chat::Chat (camelCase; used for list + preview).
type Chat struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	UserID       string     `json:"userId"`
	Model        *string    `json:"model"`
	ProjectID    *string    `json:"projectId,omitempty"`
	CreatedAt    *time.Time `json:"createdAt"`
	UpdatedAt    *time.Time `json:"updatedAt"`
	DeletedAt    *time.Time `json:"deletedAt"`
	TokenCount   *int64     `json:"tokenCount"`
	IsPersistent bool       `json:"isPersistent"`
}

// ChatResponse mirrors chat::domain::models::ChatResponse (camelCase).
type ChatResponse struct {
	ID        string        `json:"id"`
	UserID    string        `json:"userId"`
	ProjectID *string       `json:"projectId"`
	Name      string        `json:"name"`
	Messages  []ChatMessage `json:"messages"`
	Model     *string       `json:"model"`
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

// GetChatResponse mirrors chat::domain::models::GetChatResponse.
type GetChatResponse struct {
	Chat            ChatResponse `json:"chat"`
	UserAccessLevel string       `json:"userAccessLevel"`
}

// StringIDResponse mirrors model::response::StringIDResponse.
type StringIDResponse struct {
	ID string `json:"id"`
}

// --- request bodies (camelCase, matching the Rust handlers) ---

// CreateChatRequest mirrors chat::inbound::http::router::CreateChatRequest
// (serde rename_all = "camelCase").
type CreateChatRequest struct {
	Name      *string `json:"name"`
	ProjectID *string `json:"projectId"`
}

// AccessLevel strings (models_permissions AccessLevel, serde lowercase).
const (
	AccessLevelView    = "view"
	AccessLevelComment = "comment"
	AccessLevelEdit    = "edit"
	AccessLevelOwner   = "owner"
)

// accessRank orders access levels least→most, mirroring AccessLevel Ord.
func accessRank(level string) int {
	switch level {
	case AccessLevelOwner:
		return 4
	case AccessLevelEdit:
		return 3
	case AccessLevelComment:
		return 2
	case AccessLevelView:
		return 1
	default:
		return 0
	}
}

// LinkShare values (SCREAMING_SNAKE_CASE over the wire).
const (
	LinkSharePublic = "PUBLIC"
	LinkShareTeam   = "TEAM"
)

// UpdateChannelSharePermission mirrors
// models_permissions::share_permission::UpdateChannelSharePermission
// (serde rename_all = "camelCase").
type UpdateChannelSharePermission struct {
	Operation   string  `json:"operation"` // add | remove | replace
	ChannelID   string  `json:"channelId"`
	AccessLevel *string `json:"accessLevel"`
}

// UpdateSharePermissionRequestV2 mirrors
// models_permissions::share_permission::UpdateSharePermissionRequestV2. The
// link/team fields are double-options: key absent = unchanged, explicit null =
// clear, value = set.
type UpdateSharePermissionRequestV2 struct {
	linkShareSet         bool
	linkShare            *string
	linkShareLevelSet    bool
	linkShareAccessLevel *string
	teamShareLevelSet    bool
	teamShareAccessLevel *string
	ChannelSharePerms    []UpdateChannelSharePermission
	channelSharePermsSet bool
}

// UnmarshalJSON implements the double_option serde semantics.
func (u *UpdateSharePermissionRequestV2) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// The Rust struct is camelCase over the wire.
	if v, ok := raw["linkShare"]; ok {
		u.linkShareSet = true
		if string(v) != "null" {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Errorf("linkShare: %w", err)
			}
			u.linkShare = &s
		}
	}
	if v, ok := raw["linkShareAccessLevel"]; ok {
		u.linkShareLevelSet = true
		if string(v) != "null" {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Errorf("linkShareAccessLevel: %w", err)
			}
			u.linkShareAccessLevel = &s
		}
	}
	if v, ok := raw["teamShareAccessLevel"]; ok {
		u.teamShareLevelSet = true
		if string(v) != "null" {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Errorf("teamShareAccessLevel: %w", err)
			}
			u.teamShareAccessLevel = &s
		}
	}
	if v, ok := raw["channelSharePermissions"]; ok && string(v) != "null" {
		u.channelSharePermsSet = true
		if err := json.Unmarshal(v, &u.ChannelSharePerms); err != nil {
			return fmt.Errorf("channelSharePermissions: %w", err)
		}
	}
	return nil
}

// PatchChatRequest mirrors chat::inbound::http::router::PatchChatRequest
// (serde rename_all = "camelCase"). Empty projectId clears the project.
type PatchChatRequest struct {
	Name            *string                         `json:"name"`
	ProjectID       *string                         `json:"projectId"`
	SharePermission *UpdateSharePermissionRequestV2 `json:"sharePermission"`
}

// ChannelSharePermission mirrors the same-named model (snake_case).
type ChannelSharePermission struct {
	ChannelID   string `json:"channel_id"`
	AccessLevel string `json:"access_level"`
}

// SharePermissionV2 mirrors models_permissions::share_permission::SharePermissionV2
// (camelCase response).
type SharePermissionV2 struct {
	ID                      string                   `json:"id"`
	LinkShare               *string                  `json:"linkShare"`
	LinkShareAccessLevel    *string                  `json:"linkShareAccessLevel,omitempty"`
	TeamShareAccessLevel    *string                  `json:"teamShareAccessLevel"`
	Owner                   string                   `json:"owner"`
	ChannelSharePermissions []ChannelSharePermission `json:"channelSharePermissions,omitempty"`
}

// GetChatPermissionsResponse wraps the share permission for GET permissions.
type GetChatPermissionsResponse struct {
	Permissions SharePermissionV2 `json:"permissions"`
}

// ToolSet mirrors the DCS model::stream::ToolSet enum (snake_case tag "type").
type ToolSet string

const (
	ToolSetAll  ToolSet = "all"
	ToolSetNone ToolSet = "none"
)

// UnmarshalJSON accepts both `{"type":"all"}` and the plain string `"all"`.
func (t *ToolSet) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*t = ToolSet(s)
		return nil
	}
	var tagged struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &tagged); err != nil {
		return err
	}
	*t = ToolSet(tagged.Type)
	return nil
}

// orDefault mirrors `#[serde(default)]` on the Rust field: an absent/empty
// toolset means ToolSet::All.
func (t ToolSet) orDefault() ToolSet {
	if t == "" {
		return ToolSetAll
	}
	return t
}

// HttpSendChatMessageRequest mirrors the same-named Rust struct.
type SendChatMessageRequest struct {
	Content                string   `json:"content"`
	ChatID                 *string  `json:"chat_id"`
	Model                  string   `json:"model"`
	AdditionalInstructions *string  `json:"additional_instructions,omitempty"`
	Attachments            []Entity `json:"attachments,omitempty"`
	ToolSet                ToolSet  `json:"toolset"`
}

// SendChatMessageResponse mirrors the Rust response for POST
// /stream/chat/message.
type SendChatMessageResponse struct {
	StreamID  string `json:"stream_id"`
	MessageID string `json:"message_id"`
	ChatID    string `json:"chat_id"`
}

// ChatMessageError mirrors the Rust error body for the stream endpoints.
type ChatMessageError struct {
	Error    string  `json:"error"`
	StreamID *string `json:"stream_id"`
}

// StopChatStreamRequest mirrors the Rust stop request.
type StopChatStreamRequest struct {
	ChatID   string `json:"chat_id"`
	StreamID string `json:"stream_id"`
}

// StopChatStreamResponse mirrors the Rust stop response.
type StopChatStreamResponse struct {
	Stopped bool `json:"stopped"`
}

// --- stream payloads (model::stream::ChatStream, tagged "type" snake_case) ---

// ChatStreamUserMessage is the first stream item ({"type":"chat_user_message"}).
type ChatStreamUserMessage struct {
	Type        string   `json:"type"` // chat_user_message
	StreamID    string   `json:"stream_id"`
	ChatID      string   `json:"chat_id"`
	MessageID   string   `json:"message_id"`
	Content     string   `json:"content"`
	Attachments []Entity `json:"attachments"`
}

// ChatStreamMessageResponse carries one accumulated assistant part
// ({"type":"chat_message_response"}).
type ChatStreamMessageResponse struct {
	Type      string      `json:"type"` // chat_message_response
	StreamID  string      `json:"stream_id"`
	MessageID string      `json:"message_id"`
	ChatID    string      `json:"chat_id"`
	Content   MessagePart `json:"content"`
}

// ChatStreamEnd terminates the stream ({"type":"stream_end"}).
type ChatStreamEnd struct {
	Type     string `json:"type"` // stream_end
	StreamID string `json:"stream_id"`
}

// StreamError mirrors model::stream::StreamError (tag "stream_error",
// snake_case variants) wrapped in {"type":"error"}.
type StreamError struct {
	Type        string `json:"type"` // error
	StreamError string `json:"stream_error"`
	StreamID    string `json:"stream_id"`
	Model       string `json:"model,omitempty"`
}

const (
	StreamErrorProviderError        = "provider_error"
	StreamErrorModelContextOverflow = "model_context_overflow"
	StreamErrorInternalError        = "internal_error"
)

// ChatHistoryBatchMessagesRequest mirrors models_dcs::api::…Request.
type ChatHistoryBatchMessagesRequest struct {
	MessageIDs []string `json:"message_ids"`
}

// MessageWithAttachments mirrors model::chat::MessageWithAttachments.
type MessageWithAttachments struct {
	Content       string    `json:"content"`
	Date          time.Time `json:"date"`
	AttachmentIDs []string  `json:"attachmentIds"`
}

// ConversationRecord mirrors model::chat::ConversationRecord (snake_case).
type ConversationRecord struct {
	ChatID   string                   `json:"chat_id"`
	Title    string                   `json:"title"`
	Messages []MessageWithAttachments `json:"messages"`
}

// ChatHistory mirrors model::chat::ChatHistory.
type ChatHistory struct {
	Conversation []ConversationRecord `json:"conversation"`
}

// GetChatsForAttachmentResponse mirrors
// model::response::attachments::GetChatsForAttachmentResponse (snake_case).
type GetChatsForAttachmentResponse struct {
	RecentChat *Chat  `json:"recent_chat"`
	AllChats   []Chat `json:"all_chats"`
}

// ChatPreview mirrors model::chat::preview::ChatPreview (internally tagged,
// snake_case variant names).
type ChatPreview struct {
	Type      string     `json:"type"` // access | no_access | does_not_exist
	ChatID    string     `json:"chat_id"`
	ChatName  string     `json:"chat_name,omitempty"`
	Owner     string     `json:"owner,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// GetBatchPreviewRequest/Response mirror the preview handler models.
type GetBatchPreviewRequest struct {
	ChatIDs []string `json:"chat_ids"`
}

type GetBatchPreviewResponse struct {
	Previews []ChatPreview `json:"previews"`
}

// DocumentTextPart mirrors model::citations::DocumentTextPart. `reference` is
// an arbitrary JSON citation payload (DocumentReference), passed through.
type DocumentTextPart struct {
	ID         string          `json:"id"`
	DocumentID string          `json:"document_id"`
	Reference  json.RawMessage `json:"reference"`
}

// CreateIdMappingRequest/Response mirror the id_mapping handlers.
type CreateIdMappingRequest struct {
	TargetID string `json:"target_id"`
}

type CreateIdMappingResponse struct {
	Success bool `json:"success"`
}

// GetIdMappingResponse mirrors the get_mapping handler response.
type GetIdMappingResponse struct {
	TargetID *string `json:"target_id"`
}

// StructuredCompletionRequest mirrors the structured_completion handler model.
type StructuredCompletionRequest struct {
	Prompt                 string        `json:"prompt"`
	Model                  string        `json:"model"`
	OutputSchema           DynamicSchema `json:"output_schema"`
	AdditionalInstructions *string       `json:"additional_instructions,omitempty"`
	ToolSet                ToolSet       `json:"toolset"`
}

// StructuredCompletionResponse mirrors the same-named Rust struct.
type StructuredCompletionResponse struct {
	Result json.RawMessage `json:"result"`
}

// --- tool call request bodies (chat crate router) ---

type updateToolCallRequest struct {
	MessageID  string          `json:"messageId"`
	ToolCallID string          `json:"toolCallId"`
	Args       json.RawMessage `json:"args"`
}

type updateToolResponseRequest struct {
	MessageID  string          `json:"messageId"`
	ToolCallID string          `json:"toolCallId"`
	Response   json.RawMessage `json:"response"`
}

type callToolRequest struct {
	MessageID  string           `json:"messageId"`
	ToolCallID string           `json:"toolCallId"`
	Args       *json.RawMessage `json:"args"`
}

type rejectToolCallRequest struct {
	MessageID  string `json:"messageId"`
	ToolCallID string `json:"toolCallId"`
}
