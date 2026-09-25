// Chat side effects: realtime fanout over NATS (realtime.user.<id>) and
// notification requests over JetStream (notifications.ingress), replacing the
// Rust ConnectionGatewayMessages + ChannelEventDispatcher outbound adapters.
package chat

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
)

// realtimeMsg mirrors the Rust connection-gateway frame payloads published
// under comms_* message types.
type realtimeSender struct {
	Type        string  `json:"type"` // "user" | "bot"
	ID          string  `json:"id"`
	Name        *string `json:"name,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
	TriggeredBy *string `json:"triggered_by,omitempty"`
}

// effects publishes side effects for chat domain operations.
type effects struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// publishRealtime fans a payload out to every participant's
// realtime.user.<id> subject as an events.Envelope; the gateway normalizes it
// into the {type, data:string} wire shape the web client expects.
func (e *effects) publishRealtime(recipients []string, msgType string, payload any) {
	if len(recipients) == 0 || e.nc == nil {
		return
	}
	env, err := events.New(msgType, "chat", "", 1, payload)
	if err != nil {
		slog.Error("chat: realtime envelope build failed", "type", msgType, "err", err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	for _, uid := range recipients {
		if uid == "" {
			continue
		}
		if err := e.nc.Publish("realtime.user."+uid, raw); err != nil {
			slog.Warn("chat: realtime publish failed", "user_id", uid, "type", msgType, "err", err)
		}
	}
}

// commsMessage publishes the comms_message realtime frame for a mutated
// message: the MutatedMessage fields flattened + structured sender + nonce.
func (e *effects) commsMessage(recipients []string, m *MutatedMessage, nonce *string) {
	payload := map[string]any{
		"id":           m.ID,
		"channel_id":   m.ChannelID,
		"thread_id":    m.ThreadID,
		"sender_id":    m.SenderID,
		"triggered_by": m.TriggeredBy,
		"content":      m.Content,
		"created_at":   m.CreatedAt,
		"updated_at":   m.UpdatedAt,
		"edited_at":    m.EditedAt,
		"deleted_at":   m.DeletedAt,
		"sender": realtimeSender{
			Type:        "user",
			ID:          m.SenderID,
			TriggeredBy: m.TriggeredBy,
		},
	}
	if nonce != nil {
		payload["nonce"] = *nonce
	}
	e.publishRealtime(recipients, "comms_message", payload)
}

// commsAttachment publishes the comms_attachment realtime frame.
func (e *effects) commsAttachment(recipients []string, channelID, messageID uuid.UUID, atts []MessageAttachment, nonce *string) {
	payload := map[string]any{
		"channel_id":  channelID,
		"message_id":  messageID,
		"attachments": atts,
	}
	if nonce != nil {
		payload["nonce"] = *nonce
	}
	e.publishRealtime(recipients, "comms_attachment", payload)
}

// commsReaction publishes the comms_reaction realtime frame.
func (e *effects) commsReaction(recipients []string, channelID, messageID uuid.UUID, reactions []CountedReaction, nonce *string) {
	payload := map[string]any{
		"channel_id": channelID,
		"message_id": messageID,
		"reactions":  reactions,
	}
	if nonce != nil {
		payload["nonce"] = *nonce
	}
	e.publishRealtime(recipients, "comms_reaction", payload)
}

// commsTyping publishes the comms_typing realtime frame.
func (e *effects) commsTyping(recipients []string, channelID uuid.UUID, userID, action string, threadID *string, nonce *string) {
	payload := map[string]any{
		"channel_id": channelID,
		"user_id":    userID,
		"action":     action,
		"thread_id":  threadID,
	}
	if nonce != nil {
		payload["nonce"] = *nonce
	}
	e.publishRealtime(recipients, "comms_typing", payload)
}

// commsChannel publishes the comms_channel frame (channel rename/patch).
func (e *effects) commsChannel(recipients []string, ch *channelRow) {
	if ch == nil {
		return
	}
	e.publishRealtime(recipients, "comms_channel", map[string]any{
		"id":           ch.ID,
		"name":         ch.Name,
		"channel_type": ch.ChannelType,
		"org_id":       ch.OrgID,
		"created_at":   ch.CreatedAt,
		"updated_at":   ch.UpdatedAt,
	})
}

// commsChannelPicture publishes the comms_channel_picture frame.
func (e *effects) commsChannelPicture(recipients []string, channelID uuid.UUID) {
	e.publishRealtime(recipients, "comms_channel_picture", map[string]any{"channel_id": channelID})
}

// ---- bot trigger dispatch ----

// triggerPost is the camelCase payload the agentharness trigger consumer
// decodes (postedMessage in internal/api/agentharness/trigger.go).
type triggerPost struct {
	ParentType  string           `json:"parentType"`
	ParentID    string           `json:"parentId"`
	MessageID   string           `json:"messageId"`
	ThreadID    *string          `json:"threadId"`
	RootID      string           `json:"rootId"`
	Sender      string           `json:"sender"`
	TriggeredBy *string          `json:"triggeredBy"`
	Content     string           `json:"content"`
	Mentions    []triggerMention `json:"mentions"`
}

type triggerMention struct {
	EntityType string `json:"entityType"`
	EntityID   string `json:"entityId"`
}

// dispatchBotTrigger ports ChannelBotTrigger dispatch: every user-authored
// committed post is published as a `message.posted` candidate on
// agent.trigger.<channel_id> — the consumer decides whether it invokes a
// bot (explicit mention or inferred). Bot senders never dispatch, so bot
// replies can't trigger more bots (Rust: sender.as_user() guard).
func (e *effects) dispatchBotTrigger(ctx context.Context, channelID uuid.UUID, m *MutatedMessage, mentions []SimpleMention) {
	if e.js == nil {
		return
	}
	if len(m.SenderID) > 4 && m.SenderID[:4] == "bot|" {
		return // bots do not trigger bots
	}
	ms := make([]triggerMention, 0, len(mentions))
	for _, mt := range mentions {
		if mt.EntityType != "" && mt.EntityID != "" {
			ms = append(ms, triggerMention{EntityType: mt.EntityType, EntityID: mt.EntityID})
		}
	}
	root := m.ID
	var threadID *string
	if m.ThreadID != nil {
		t := m.ThreadID.String()
		threadID = &t
		root = *m.ThreadID
	}
	post := triggerPost{
		ParentType:  "channel",
		ParentID:    channelID.String(),
		MessageID:   m.ID.String(),
		ThreadID:    threadID,
		RootID:      root.String(),
		Sender:      m.SenderID,
		TriggeredBy: m.TriggeredBy,
		Content:     m.Content,
		Mentions:    ms,
	}
	env, err := events.New("message.posted", "chat", events.Shard(events.StreamAgentTrig, channelID.String()), 1, post)
	if err != nil {
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	if _, err := e.js.Publish(ctx, events.Shard(events.StreamAgentTrig, channelID.String()), raw); err != nil {
		slog.Error("chat: bot trigger publish failed", "channel_id", channelID, "err", err)
	}
}

// publishDeleteChatJob queues the hard-delete job consumed by
// internal/jobs (subject jobs.delete_chat), mirroring the Rust enqueue.
func (s *Server) publishDeleteChatJob(ctx context.Context, channelID uuid.UUID) {
	if s.fx == nil || s.fx.js == nil {
		return
	}
	env, err := events.New("chat.delete", "chat", "jobs.delete_chat", 1,
		map[string]any{"chat_id": channelID.String()})
	if err != nil {
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	if _, err := s.fx.js.Publish(ctx, "jobs.delete_chat", raw); err != nil {
		slog.Error("chat: delete_chat job publish failed", "channel_id", channelID, "err", err)
	}
}

// ---- notification ingress ----

// notificationEntity is the model_entity::Entity wire shape
// {entity_type, entity_id} used inside notification requests.
type notificationEntity struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
}

// taggedContent is the adjacently-tagged notification metadata
// ({tag, content}) stored on notification rows.
type taggedContent struct {
	Tag     string         `json:"tag"`
	Content map[string]any `json:"content"`
}

// ingressRequest mirrors notification.SendRequest as consumed by the
// notification ingress consumer.
type ingressRequest struct {
	Req struct {
		NotificationEntity          notificationEntity  `json:"notification_entity"`
		SecondaryNotificationEntity *notificationEntity `json:"secondary_notification_entity"`
		Notification                taggedContent       `json:"notification"`
		SenderID                    *string             `json:"sender_id"`
		RecipientIDs                []string            `json:"recipient_ids"`
	} `json:"req"`
	UUIDToWrite     uuid.UUID `json:"uuid_to_write"`
	SendConnGateway bool      `json:"send_conn_gateway"`
	BuildApns       *struct {
		Notif map[string]any `json:"notif"`
		Attr  struct {
			PushType    string `json:"push_type"`
			CollapseKey string `json:"collapse_key"`
		} `json:"attr"`
	} `json:"build_apns"`
}

// sendIngress publishes a notification send request to
// notifications.ingress. channelType/channelName fill CommonChannelMetadata
// (camelCase on the wire).
func (e *effects) sendIngress(ctx context.Context, ch *channelRow, senderID *string, recipients []string, secondary *notificationEntity, tag string, content map[string]any, pushTitle, pushBody string) {
	if len(recipients) == 0 || e.js == nil {
		return
	}
	content["channelType"] = string(ch.ChannelType)
	name := ""
	if ch.Name != nil {
		name = *ch.Name
	}
	content["channelName"] = name

	var req ingressRequest
	req.Req.NotificationEntity = notificationEntity{EntityType: "channel", EntityID: ch.ID.String()}
	req.Req.SecondaryNotificationEntity = secondary
	req.Req.Notification = taggedContent{Tag: tag, Content: content}
	req.Req.SenderID = senderID
	req.Req.RecipientIDs = recipients
	req.UUIDToWrite = uuid.New()
	req.SendConnGateway = true

	// BuildApns mirrors Rust .with_apns(): an Alert push whose aps dict
	// carries title+body and the notification data flattened beside it.
	var apns ingressRequest
	_ = apns // shape reference
	req.BuildApns = &struct {
		Notif map[string]any `json:"notif"`
		Attr  struct {
			PushType    string `json:"push_type"`
			CollapseKey string `json:"collapse_key"`
		} `json:"attr"`
	}{
		Notif: map[string]any{
			"aps": map[string]any{
				"alert": map[string]any{"title": pushTitle, "body": pushBody},
				"sound": "default",
			},
			"notification_entity": map[string]any{"entity_type": "channel", "entity_id": ch.ID.String()},
		},
	}
	req.BuildApns.Attr.PushType = "Alert"

	raw, err := json.Marshal(struct {
		Request ingressRequest `json:"request"`
	}{Request: req})
	if err != nil {
		return
	}
	if _, err := e.js.Publish(ctx, events.StreamNotifIngress, raw); err != nil {
		slog.Error("chat: notification ingress publish failed", "channel_id", ch.ID, "err", err)
	}
}

// displayName falls back to the email local part like the Rust
// fallback_user_name helper.
func displayName(userID string) string {
	const prefix = "macro|"
	s := userID
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		s = s[len(prefix):]
	}
	if i := indexByte(s, '@'); i > 0 {
		return s[:i]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
