// Package natsx centralizes NATS connection and JetStream bootstrap helpers.
package natsx

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Connect dials NATS and returns the connection plus a JetStream context.
func Connect(ctx context.Context, url string) (*nats.Conn, jetstream.JetStream, error) {
	nc, err := nats.Connect(url,
		nats.Name("macro"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500*time.Millisecond),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("nats connect %s: %w", url, err)
	}
	// Fail fast: RetryOnFailedConnect can return a conn that is still
	// reconnecting; round-trip a ping so misconfigured NATS_URL fails at boot.
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("nats ping %s: %w", url, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("jetstream init: %w", err)
	}
	go func() {
		<-ctx.Done()
		nc.Drain()
	}()
	return nc, js, nil
}

// EnsureStream creates or updates a stream with the given subjects.
func EnsureStream(ctx context.Context, js jetstream.JetStream, name string, subjects []string) (jetstream.Stream, error) {
	return js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
		MaxAge:    7 * 24 * time.Hour,
	})
}

// EnsureEventStream is like EnsureStream but keeps history (LimitsPolicy)
// for event topics that replace Kafka (replayable).
func EnsureEventStream(ctx context.Context, js jetstream.JetStream, name string, subjects []string) (jetstream.Stream, error) {
	return js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		MaxAge:    30 * 24 * time.Hour,
	})
}

// EnsureConsumer creates or updates a durable pull consumer.
func EnsureConsumer(ctx context.Context, s jetstream.Stream, durable, filterSubject string) (jetstream.Consumer, error) {
	return s.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: filterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
	})
}
