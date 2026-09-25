package search

import (
	"context"
	"encoding/json"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
)

// SearchEventPublisher mirrors domain::ports::SearchEventPublisher — batches
// of queue messages onto the search work stream (SQS → JetStream).
type SearchEventPublisher interface {
	Publish(ctx context.Context, messages []QueueMessage) error
}

// NATSPublisher wraps each queue message in an events.Envelope of type
// "search.queue_message" and publishes it to search.index.<primary_id> on
// the search.index stream (work-queue replacement for the SQS search event
// queue; the stream is replayable, indexing is idempotent).
type NATSPublisher struct {
	JS jetstream.JetStream
	NC *nats.Conn // core fallback when JetStream is unavailable
}

// Publish implements SearchEventPublisher.
func (p NATSPublisher) Publish(ctx context.Context, messages []QueueMessage) error {
	for _, m := range messages {
		env, err := events.New(TypeQueueMessage, "search", m.PrimaryID(), 1, m)
		if err != nil {
			return err
		}
		data, err := json.Marshal(env)
		if err != nil {
			return err
		}
		subject := events.Shard(events.StreamSearch, m.PrimaryID())
		if p.JS != nil {
			if _, err := p.JS.Publish(ctx, subject, data); err != nil {
				return err
			}
		} else if p.NC != nil {
			if err := p.NC.Publish(subject, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// TypeQueueMessage is the envelope type wrapping a serialized QueueMessage.
const TypeQueueMessage = "search.queue_message"

// publishEnvelope writes one envelope to search.index.<aggregate_id> —
// JetStream when available, core NATS otherwise (extract_sync path).
func publishEnvelope(ctx context.Context, p NATSPublisher, aggregateID string, env events.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	subject := events.Shard(events.StreamSearch, aggregateID)
	if p.JS != nil {
		_, err := p.JS.Publish(ctx, subject, data)
		return err
	}
	if p.NC != nil {
		return p.NC.Publish(subject, data)
	}
	return nil
}
