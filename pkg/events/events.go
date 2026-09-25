// Package events defines the event envelope and subject catalog.
// Replaces macro_event_broker / Kafka topic naming.
package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Envelope is the single JSON envelope for every message on the bus.
type Envelope struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`    // e.g. "chat.message_sent"
	Source  string          `json:"source"`  // producing service, e.g. "chat"
	Subject string          `json:"subject"` // aggregate key for ordering
	Time    time.Time       `json:"time"`
	Version int             `json:"version"` // schema version
	Data    json.RawMessage `json:"data"`
}

func New(typ, source, subject string, version int, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		ID:      uuid.NewString(),
		Type:    typ,
		Source:  source,
		Subject: subject,
		Time:    time.Now().UTC(),
		Version: version,
		Data:    raw,
	}, nil
}

// Subject catalog. Work-queue streams (ex-SQS) use WorkQueuePolicy;
// event streams (ex-Kafka) keep history for replay.
const (
	// Event streams (ex-Kafka topics). Subject-sharded per aggregate:
	// "<stream>.<aggregate_id>" preserves per-key ordering.
	StreamChats     = "macro.chats"
	StreamDocuments = "macro.documents"
	StreamTeams     = "macro.teams"
	StreamAgentTrig = "agent.trigger" // subject: agent.trigger.<channel_id>
	StreamSearch    = "search.index"

	// Work-queue streams (ex-SQS / ex-Lambda triggers).
	StreamJobs          = "jobs" // generic lambda-replacement jobs
	StreamNotifIngress  = "notifications.ingress"
	StreamNotifDelivery = "notifications.delivery"
	StreamPushEvents    = "notifications.push_events"
	StreamUpload        = "upload.extract"

	// Core pub/sub subjects for realtime fanout (no persistence).
	// NOTE: user ids are principals like "macro|a@b.com" and contain dots, so
	// subscriptions must use the multi-token wildcard `>`, never `*`.
	SubjectRealtimeUser = "realtime.user.>" // realtime.user.<user_id>
	SubjectNotifStatus  = "notifications.status.>"
)

// Shard returns the subject-sharded name for an event stream publish,
// e.g. Shard(StreamChats, chatID) = "macro.chats.<chat_id>".
// The aggregate id must be subject-safe (no '.', '*', '>', or whitespace)
// or the shard would corrupt wildcard subscriptions and per-key ordering.
func Shard(stream, aggregateID string) string {
	for _, r := range aggregateID {
		if r == '.' || r == '*' || r == '>' || r == ' ' || r == '\t' {
			return stream + "._invalid"
		}
	}
	return stream + "." + aggregateID
}
