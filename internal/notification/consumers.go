package notification

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
)

// StreamNotif is the JetStream stream name (dots are invalid in stream
// names); the events.* constants are subjects within it.
const StreamNotif = "notifications"

// EnsureStreams provisions the notification work-queue stream collecting the
// ingress, delivery, and push-event subjects.
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	_, err := natsx.EnsureStream(ctx, js, StreamNotif, []string{
		events.StreamNotifIngress,
		events.StreamNotifDelivery,
		events.StreamPushEvents,
	})
	return err
}

// decodePayload unmarshals a JetStream message body into out, tolerating both
// raw payloads and events.Envelope wrapping ({"data": {...}}).
func decodePayload(data []byte, out any) error {
	var probe struct {
		Data json.RawMessage `json:"data"`
		Type string          `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err == nil && len(probe.Data) > 0 && probe.Type != "" {
		return json.Unmarshal(probe.Data, out)
	}
	return json.Unmarshal(data, out)
}

// nakDelay is the redelivery delay for transient handler failures — the
// JetStream analogue of the SQS visibility timeout. Plain Nak() redelivers
// almost immediately, which would hot-loop on an outage (e.g. Postgres
// down); a delayed NAK paces retries instead.
const nakDelay = 2 * time.Second

// consume runs a durable pull-consumer loop on a subject inside the
// notifications work-queue stream.
// handler returning nil → ack; error → nak-with-delay (redelivery up to
// MaxDeliver). Handlers must return nil for terminal/malformed messages so
// they are acked and discarded rather than retried forever.
func consume(ctx context.Context, js jetstream.JetStream, subject, durable, name string, handler func(context.Context, []byte) error) error {
	if err := EnsureStreams(ctx, js); err != nil {
		return err
	}
	s, err := js.Stream(ctx, StreamNotif)
	if err != nil {
		return err
	}
	cons, err := natsx.EnsureConsumer(ctx, s, durable, subject)
	if err != nil {
		return err
	}
	slog.Info("consumer started", "consumer", name, "subject", subject, "durable", durable)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		batch, err := cons.Fetch(10, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("fetch failed", "consumer", name, "err", err)
			if !sleep(ctx, time.Second) {
				return ctx.Err()
			}
			continue
		}
		for msg := range batch.Messages() {
			if err := handler(ctx, msg.Data()); err != nil {
				slog.Error("message failed", "consumer", name, "err", err)
				_ = msg.NakWithDelay(nakDelay)
				continue
			}
			_ = msg.Ack()
		}
		if err := batch.Error(); err != nil {
			slog.Error("batch error", "consumer", name, "err", err)
		}
	}
}

// ingressConsumer consumes notifications.ingress (ex-SQS ingress queue):
// each message is a producer's send request → filter + persist + fan out to
// notifications.delivery.
func ingressConsumer(ctx context.Context, js jetstream.JetStream, svc *Service) error {
	return consume(ctx, js, events.StreamNotifIngress, "notification-ingress", "ingress",
		func(ctx context.Context, data []byte) error {
			var msg IngressMessage
			if err := decodePayload(data, &msg); err != nil {
				// Malformed payloads are terminal — ack them away.
				slog.Error("bad ingress message", "err", err)
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			_, _, err := svc.SendNotification(ctx, msg.Request)
			return err
		})
}

// deliveryConsumer consumes notifications.delivery (ex-SQS delivery queue):
// realtime first → push for offline users → digest email fallback.
func deliveryConsumer(ctx context.Context, js jetstream.JetStream, d *Delivery) error {
	return consume(ctx, js, events.StreamNotifDelivery, "notification-delivery", "delivery",
		func(ctx context.Context, data []byte) error {
			var msg DeliveryMessage
			if err := decodePayload(data, &msg); err != nil {
				slog.Error("bad delivery message", "err", err)
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			return d.Deliver(ctx, msg)
		})
}

// pushEventConsumer consumes notifications.push_events (ex-SNS event queue):
// provider feedback — delivery failures and endpoint deletions.
func pushEventConsumer(ctx context.Context, js jetstream.JetStream, h *PushEventHandler) error {
	return consume(ctx, js, events.StreamPushEvents, "notification-push-events", "push_events",
		func(ctx context.Context, data []byte) error {
			var ev PushEvent
			if err := decodePayload(data, &ev); err != nil {
				slog.Error("bad push event", "err", err)
				return nil
			}
			return h.Handle(ctx, ev)
		})
}
