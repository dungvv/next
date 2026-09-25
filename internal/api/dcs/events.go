// Sanitized chat lifecycle events published to JetStream on
// macro.chats.<chat_id>. Payloads deliberately exclude message content,
// attachment content, and share-permission payloads — only ids, names, roles,
// and counts, matching crates/chat/src/domain/events.rs.
package dcs

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
)

// Event types on the macro.chats stream (Rust ChatTopicEvent names).
const (
	EventChatCreated            = "chat.created"
	EventChatUpdated            = "chat.updated"
	EventChatDeleted            = "chat.deleted"
	EventChatPermanentlyDeleted = "chat.permanently_deleted"
	EventChatRestored           = "chat.restored"
	EventChatCopied             = "chat.copied"
	EventChatMessageSent        = "chat.message_sent"
	EventChatMessageDeleted     = "chat.message_deleted"
)

const eventSource = "dcs"

// --- metadata payloads (snake_case, mirroring the Rust *Metadata structs) ---

type chatCreatedMeta struct {
	ChatID    string  `json:"chat_id"`
	Owner     string  `json:"owner"`
	Name      string  `json:"name"`
	ProjectID *string `json:"project_id"`
}

type chatUpdatedMeta struct {
	ChatID                string  `json:"chat_id"`
	ActorUserID           string  `json:"actor_user_id"`
	Name                  *string `json:"name"`
	PreviousProjectID     *string `json:"previous_project_id"`
	ProjectID             *string `json:"project_id"`
	SharePermissionUpdate bool    `json:"share_permission_updated"`
}

type chatDeletedMeta struct {
	ChatID      string  `json:"chat_id"`
	ActorUserID *string `json:"actor_user_id"`
	ProjectID   *string `json:"project_id"`
}

type chatPermanentlyDeletedMeta struct {
	ChatID      string  `json:"chat_id"`
	ActorUserID *string `json:"actor_user_id"`
	ProjectID   *string `json:"project_id"`
}

type chatRestoredMeta struct {
	ChatID      string  `json:"chat_id"`
	ActorUserID *string `json:"actor_user_id"`
	ProjectID   *string `json:"project_id"`
}

type chatCopiedMeta struct {
	ChatID       string `json:"chat_id"`
	SourceChatID string `json:"source_chat_id"`
	Owner        string `json:"owner"`
	Name         string `json:"name"`
}

type chatMessageSentMeta struct {
	ChatID          string  `json:"chat_id"`
	MessageID       string  `json:"message_id"`
	Role            string  `json:"role"`
	Model           string  `json:"model"`
	ActorUserID     *string `json:"actor_user_id"`
	AttachmentCount int     `json:"attachment_count"`
}

type chatMessageDeletedMeta struct {
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id"`
}

// Publisher emits chat lifecycle events to JetStream. A nil publisher (NATS
// down) is a no-op — events are fire-and-forget, never blocking requests.
type Publisher struct {
	JS jetstream.JetStream
}

// publish wraps the metadata in the shared events.Envelope and publishes it
// to macro.chats.<chat_id> so per-chat ordering is preserved.
func (p *Publisher) publish(ctx context.Context, chatID, typ string, meta any) {
	if p == nil || p.JS == nil {
		return
	}
	subject := events.Shard(events.StreamChats, chatID)
	env, err := events.New(typ, eventSource, subject, 1, meta)
	if err != nil {
		slog.Error("dcs: marshal chat event", "type", typ, "chat", chatID, "err", err)
		return
	}
	payload, err := json.Marshal(env)
	if err != nil {
		slog.Error("dcs: marshal envelope", "type", typ, "chat", chatID, "err", err)
		return
	}
	if _, err := p.JS.Publish(ctx, subject, payload); err != nil {
		slog.Error("dcs: publish chat event", "type", typ, "chat", chatID, "err", err)
	}
}
