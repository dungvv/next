package contacts

import (
	"context"
	"log/slog"
	"time"

	"github.com/macro-inc/macro/pkg/natsx"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream and consumer settings for the contacts ingress queue — the JetStream
// replacement for the Rust ContactsQueue SQS worker.
const (
	// StreamIngress is the work-queue stream name for contacts messages.
	StreamIngress = "contacts"
	// SubjectIngress is the subject producers publish to.
	SubjectIngress = "contacts.ingress"
	// ConsumerIngress is the durable consumer name.
	ConsumerIngress = "contacts-api"
)

// RunConsumer starts a JetStream pull consumer on StreamIngress and applies
// each message through the service. It replaces the SQSWorker in
// contacts_service's main.rs. Blocks until ctx is cancelled.
func RunConsumer(ctx context.Context, js jetstream.JetStream, svc *Service) error {
	stream, err := natsx.EnsureStream(ctx, js, StreamIngress, []string{SubjectIngress})
	if err != nil {
		return err
	}
	cons, err := natsx.EnsureConsumer(ctx, stream, ConsumerIngress, SubjectIngress)
	if err != nil {
		return err
	}
	slog.Info("contacts: consumer started", "stream", StreamIngress, "subject", SubjectIngress)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := cons.Fetch(10, jetstream.FetchMaxWait(10*time.Second))
		if err != nil {
			slog.Error("contacts: fetch failed", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		for msg := range msgs.Messages() {
			parsed := ParseMessage(msg.Data())
			if parsed == nil {
				slog.Warn("contacts: message body could not be parsed as ContactsMessage")
				_ = msg.Ack()
				continue
			}
			if err := svc.ProcessMessage(ctx, parsed); err != nil {
				slog.Error("contacts: error processing message", "err", err)
				_ = msg.Nak()
				continue
			}
			_ = msg.Ack()
		}
		if err := msgs.Error(); err != nil {
			slog.Error("contacts: fetch error", "err", err)
		}
	}
}
