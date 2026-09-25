package search

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
	pkgsearch "github.com/macro-inc/macro/pkg/search"
)

// ConsumerDurable is the JetStream durable name (replaces the
// "search-processing-service" Kafka consumer group).
const ConsumerDurable = "search-processing"

// EnsureSearchStream creates the replayable search.index event stream
// (ex-Kafka topic) plus the durable processing consumer.
func EnsureSearchStream(ctx context.Context, js jetstream.JetStream) error {
	stream, err := natsx.EnsureEventStream(ctx, js, events.StreamSearch,
		[]string{events.StreamSearch + ".>"})
	if err != nil {
		return err
	}
	_, err = natsx.EnsureConsumer(ctx, stream, ConsumerDurable, events.StreamSearch+".>")
	return err
}

// RunConsumer drains the search.index durable, dispatching each envelope to
// the indexer. Mirrors inbound::kafka_consumer: per-entity events become the
// same index ops the queue messages do; malformed records are acked so they
// can't wedge the stream.
func RunConsumer(ctx context.Context, js jetstream.JetStream, ix *Indexer) error {
	if js == nil {
		return errors.New("search: JetStream unavailable")
	}
	if err := EnsureSearchStream(ctx, js); err != nil {
		return err
	}
	stream, err := js.Stream(ctx, events.StreamSearch)
	if err != nil {
		return err
	}
	cons, err := stream.Consumer(ctx, ConsumerDurable)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		msgs, err := cons.Fetch(10, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("search: fetch failed", "err", err)
			time.Sleep(time.Second)
			continue
		}
		for msg := range msgs.Messages() {
			if err := ix.ProcessEnvelope(ctx, msg.Data()); err != nil {
				// The Kafka consumer retried in-process then dropped; the
				// JetStream MaxDeliver policy gives the same bounded retry.
				slog.Error("search: process failed", "subject", msg.Subject(), "err", err)
				_ = msg.Nak()
				continue
			}
			if err := msg.Ack(); err != nil {
				slog.Error("search: ack failed", "err", err)
			}
		}
		if err := msgs.Error(); err != nil {
			slog.Error("search: fetch error", "err", err)
		}
	}
}

// ProcessEnvelope decodes one bus envelope and routes it: search queue
// messages go to Process; broker events are mapped onto the same index ops
// by entity prefix.
func (i *Indexer) ProcessEnvelope(ctx context.Context, data []byte) error {
	var env events.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		// Not an envelope — try the bare queue message shape for producers
		// that publish raw SearchQueueMessage JSON.
		var qm QueueMessage
		if err := json.Unmarshal(data, &qm); err == nil && qm.Kind != "" {
			return i.Process(ctx, qm)
		}
		return nil // malformed: ack-worthy
	}
	if env.Type == TypeQueueMessage {
		var qm QueueMessage
		if err := json.Unmarshal(env.Data, &qm); err != nil {
			return nil // malformed payload: ack
		}
		return i.Process(ctx, qm)
	}
	return i.processBrokerEvent(ctx, env)
}

// dataID pulls a string field out of the event's data payload.
func dataID(data json.RawMessage, keys ...string) string {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// processBrokerEvent maps a broker event (the ex-Kafka MacroEvent types)
// onto index operations. Event subjects carry the aggregate id; handlers
// re-fetch full state from Postgres, so we only need the id.
//
// TODO(event-parity): the Rust kafka_consumer handled a fixed set of typed
// events (DocumentMacroEvent, ChatMacroEvent, ChannelMacroEvent,
// EmailMacroEvent, CallMacroEvent, ProjectMacroEvent, CalendarMacroEvent,
// PropertyMacroEvent, AgentSessionLifecycleMacroEvent) with per-event field
// extraction. This prefix mapping covers upserts/deletes by aggregate id;
// events whose payload carries a different entity id than the subject (chat
// messages, channel messages, email threads) still need exact field mapping.
func (i *Indexer) processBrokerEvent(ctx context.Context, env events.Envelope) error {
	typ := env.Type
	subject := env.Subject
	// Deletion event suffixes used across the Rust broker events.
	deleted := strings.Contains(typ, "delet") || strings.Contains(typ, "remov")

	switch {
	case strings.HasPrefix(typ, "document."):
		if subject == "" {
			subject = dataID(env.Data, "document_id", "entity_id")
		}
		if subject == "" {
			return nil
		}
		if deleted {
			return i.Store.Delete(ctx, pkgsearch.EntityDocument, subject)
		}
		return i.indexDocument(ctx, SearchExtractorMessage{DocumentID: subject})
	case strings.HasPrefix(typ, "project."):
		if subject == "" {
			subject = dataID(env.Data, "project_id", "entity_id")
		}
		if subject == "" {
			return nil
		}
		if deleted {
			return i.Store.Delete(ctx, pkgsearch.EntityProject, subject)
		}
		return i.indexProject(ctx, UpsertProject{ProjectID: subject})
	case strings.HasPrefix(typ, "calendar."):
		if subject == "" {
			subject = dataID(env.Data, "event_id", "entity_id")
		}
		if subject == "" {
			return nil
		}
		if deleted {
			return i.Store.Delete(ctx, pkgsearch.EntityCalendarEvent, subject)
		}
		return i.indexCalendarEvent(ctx, UpsertCalendarEvent{EventID: subject})
	case strings.HasPrefix(typ, "call."):
		if subject == "" {
			subject = dataID(env.Data, "call_id", "entity_id")
		}
		if subject == "" {
			return nil
		}
		if deleted {
			return i.Store.Delete(ctx, pkgsearch.EntityCall, subject)
		}
		return i.indexCall(ctx, CallRecordMessage{CallID: subject})
	case strings.HasPrefix(typ, "chat."):
		// Chat events key on the chat id; the message id lives in data.
		chatID, msgID := subject, dataID(env.Data, "message_id")
		if msgID == "" {
			return nil // not a per-message event; nothing indexable
		}
		if deleted {
			return i.Store.Delete(ctx, pkgsearch.EntityChatMessage, msgID)
		}
		return i.indexChatMessage(ctx, ChatMessagePayload{ChatID: chatID, MessageID: msgID})
	case strings.HasPrefix(typ, "channel."):
		channelID, msgID := subject, dataID(env.Data, "message_id")
		if channelID == "" {
			channelID = dataID(env.Data, "channel_id")
		}
		if msgID == "" || channelID == "" {
			return nil
		}
		if deleted {
			return i.Store.Delete(ctx, pkgsearch.EntityChannelMessage, msgID)
		}
		return i.indexChannelMessage(ctx, ChannelMessageUpdate{ChannelID: channelID, MessageID: msgID})
	case strings.HasPrefix(typ, "email."):
		threadID := dataID(env.Data, "thread_id")
		if threadID == "" {
			threadID = subject
		}
		if threadID == "" {
			return nil
		}
		if deleted {
			return i.Store.Delete(ctx, pkgsearch.EntityEmailThread, threadID)
		}
		macroUser := dataID(env.Data, "macro_user_id", "user_id")
		if macroUser == "" && i.Email != nil {
			// Resolve the owner from the link when the event omits it.
			_ = i.Email.QueryRow(ctx, `
				SELECT l.macro_id FROM email_threads t
				JOIN email_links l ON t.link_id = l.id
				WHERE t.id = $1::uuid`, threadID).Scan(&macroUser)
		}
		if macroUser == "" {
			return nil
		}
		id, err := uuid.Parse(threadID)
		if err != nil {
			return nil
		}
		return i.indexEmailThread(ctx, id, macroUser)
	case strings.HasPrefix(typ, "property."):
		entityID := dataID(env.Data, "entity_id")
		entityType := dataID(env.Data, "entity_type")
		if entityID == "" || entityType == "" {
			return nil
		}
		return PgPropertyIndexer{Indexer: i}.Reindex(ctx, entityID, entityType)
	default:
		// agent_session lifecycle + anything else: no index op yet.
		slog.Debug("search: broker event ignored", "type", typ)
		return nil
	}
}
