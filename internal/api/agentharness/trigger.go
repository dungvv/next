// The agent trigger consumer: the JetStream port of
// services/agent_trigger_service, which in Rust reads committed-post events
// off Kafka and publishes agent-session events for the harness service.
//
// In the combined Go binary the pipeline collapses to one hop: this consumer
// evaluates each committed post on the agent.trigger stream and opens the
// session in-process, instead of publishing to a second topic for a separate
// consumer. Ordering is preserved the same way the Rust deployment got it
// from Kafka partitions: subjects are sharded per channel
// (agent.trigger.<channel_id>) and this consumer is a single sequential pull
// loop, so posts from one channel are evaluated in publish order.
package agentharness

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
)

// triggerSubjects is the stream's subject shard pattern.
const triggerSubjects = events.StreamAgentTrig + ".>"

// postedMessage is the consumer-side decode of a committed post — the
// subset of messages::domain::events::MessagePostedMetadata trigger
// evaluation uses.
type postedMessage struct {
	ParentType  string  `json:"parentType"` // "channel" | "document"
	ParentID    string  `json:"parentId"`
	ChannelID   string  `json:"channelId"` // legacy producers
	MessageID   string  `json:"messageId"`
	ThreadID    *string `json:"threadId"`
	RootID      string  `json:"rootId"`
	Sender      string  `json:"sender"` // "macro|<email>" | "bot|<uuid>"
	TriggeredBy *string `json:"triggeredBy"`
	Content     string  `json:"content"`
	Mentions    []struct {
		EntityType string `json:"entityType"`
		EntityID   string `json:"entityId"`
	} `json:"mentions"`
}

// RunTriggerConsumer consumes agent.trigger until ctx is cancelled.
// Durable pull consumer, explicit ack, redelivery via Nak on retryable
// failure — the semantics the Kafka consumer group provided.
func (s *Service) RunTriggerConsumer(ctx context.Context, js jetstream.JetStream) error {
	stream, err := natsx.EnsureEventStream(ctx, js, events.StreamAgentTrig, []string{triggerSubjects})
	if err != nil {
		return err
	}
	cons, err := natsx.EnsureConsumer(ctx, stream, s.cfg.TriggerConsumer, triggerSubjects)
	if err != nil {
		return err
	}
	slog.Info("agentharness: trigger consumer started",
		"stream", events.StreamAgentTrig, "durable", s.cfg.TriggerConsumer)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := cons.Fetch(10, jetstream.FetchMaxWait(10*time.Second))
		if err != nil {
			slog.Error("agentharness: trigger fetch failed", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		// Sequential processing is deliberate: subject-sharded publish means
		// a single in-order consumer preserves per-channel ordering exactly.
		for msg := range msgs.Messages() {
			if err := s.processTrigger(ctx, msg.Data()); err != nil {
				slog.Error("agentharness: trigger processing failed", "err", err)
				_ = msg.Nak()
				continue
			}
			_ = msg.Ack()
		}
		if err := msgs.Error(); err != nil {
			slog.Error("agentharness: trigger fetch error", "err", err)
		}
	}
}

// processTrigger evaluates one committed post. Undecodable payloads are
// dropped successfully (ack'd) — matching the Rust consumer, which commits
// the offset rather than wedging the partition on a poison record.
func (s *Service) processTrigger(ctx context.Context, data []byte) error {
	var env events.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		slog.Warn("agentharness: dropping undecodable trigger event", "err", err)
		return nil
	}
	if !strings.HasSuffix(env.Type, "posted") && env.Type != "message.posted" {
		return nil // not a post event; nothing to evaluate
	}
	var post postedMessage
	if err := json.Unmarshal(env.Data, &post); err != nil {
		slog.Warn("agentharness: dropping undecodable post payload", "err", err)
		return nil
	}
	return s.evaluatePost(ctx, post)
}

// evaluatePost is the usable-core port of AgentTriggerService::evaluate:
// explicit bot mentions only. Implicit triggers (an unmentioned reply in a
// session's thread judged by a fast model) need the thread-history and model
// ports that are not yet ported — TODO(trigger): add them.
func (s *Service) evaluatePost(ctx context.Context, post postedMessage) error {
	if strings.HasPrefix(post.Sender, "bot|") {
		return nil // bots do not trigger bots; no relay loops
	}
	owner := post.Sender
	if post.TriggeredBy != nil && *post.TriggeredBy != "" {
		owner = *post.TriggeredBy
	}
	channelID := post.ParentID
	if post.ParentType == "" && post.ChannelID != "" {
		post.ParentType = "channel"
		channelID = post.ChannelID
	}
	threadID := post.ThreadID
	if threadID == nil && post.RootID != "" {
		threadID = &post.RootID
	}

	for _, botID := range mentionedBotIDs(post) {
		facts, err := botFacts(ctx, s.repo.db, botID)
		if err != nil {
			return err
		}
		if facts == nil || !facts.HasAgent {
			continue
		}
		if threadID != nil {
			existing, err := s.repo.FindForThread(ctx, *threadID, botID)
			if err != nil {
				return err
			}
			if existing != nil {
				// Thread already routes to a session: the mention still goes
				// to the runtime as a prompt on that session.
				frame := map[string]any{"type": "prompt", "prompt": post.Content}
				if err := s.dispatch(ctx, existing, uuid.NewString(), post.Sender, frame); err != nil {
					return err
				}
				continue
			}
		}
		req := CreateSessionRequest{
			BotID:  &botID,
			Prompt: &post.Content,
			Thread: &CreateSessionThread{
				ParentType: post.ParentType,
				ParentID:   strptr(channelID),
				ThreadID:   threadID,
				MessageID:  post.MessageID,
				Content:    post.Content,
			},
		}
		caller := auth.Caller{UserID: owner, Internal: true}
		_, err = s.Create(ctx, caller, req)
		var tee *ThreadExistsError
		switch {
		case err == nil:
			slog.Info("agentharness: mention opened a session",
				"bot", botID, "message", post.MessageID)
		case errors.As(err, &tee):
			// Raced with another creator: fine, the thread routes there.
		case errors.Is(err, ErrUnarmed):
			slog.Warn("agentharness: managed runtime unarmed; session recorded without sandbox",
				"bot", botID)
		default:
			return err
		}
	}
	return nil
}

// mentionedBotIDs extracts the bots a post explicitly mentions — the port of
// messages::mentions::bot_mention_ids: entity_type "bot" with a bot|<uuid>
// id, or a "user" mention of the Macro AI bot (which answers in-channel and
// opens no session, so it is excluded here).
func mentionedBotIDs(post postedMessage) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range post.Mentions {
		if m.EntityType != "bot" {
			continue
		}
		id, ok := strings.CutPrefix(m.EntityID, "bot|")
		if !ok {
			continue
		}
		if id == MacroAIBotID || id == MacroSystemBotID {
			continue
		}
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

func strptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
