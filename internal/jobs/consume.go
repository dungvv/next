package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
)

const (
	// fetchBatchSize is how many messages a consumer pulls per Fetch call.
	// It also bounds how many messages are processed concurrently.
	fetchBatchSize = 16
	// fetchMaxWait bounds a single Fetch call so ctx cancellation is honored
	// promptly and the loop can re-check for shutdown.
	fetchMaxWait = 5 * time.Second
	// maxDeliver mirrors natsx.EnsureConsumer's ConsumerConfig.MaxDeliver.
	// Keep in sync with pkg/natsx (that package owns the consumer config).
	maxDeliver = 5
	// inProgressInterval is how often a running handler heartbeats its
	// in-flight message so handlers slower than AckWait are not redelivered.
	inProgressInterval = 10 * time.Second
	// nakBackoffBase / nakBackoffMax bound the exponential redelivery delay
	// applied on handler errors (was: immediate SQS redelivery after the
	// 30s visibility timeout; a delay avoids hot retry loops).
	nakBackoffBase = 5 * time.Second
	nakBackoffMax  = 2 * time.Minute
)

// Handler processes one JetStream message carrying an events.Envelope.
// Returning nil Acks the message; a non-nil error Naks it for redelivery
// (Term on the final allowed delivery).
type Handler func(ctx context.Context, env events.Envelope) error

// Consume ensures the work-queue stream and a durable pull consumer exist,
// then loops fetching batches and invoking handler per message until ctx is
// cancelled.
//
//   - handler success  -> Ack
//   - handler error    -> NakWithDelay (exponential backoff, see nakDelay)
//   - handler error on the final allowed delivery -> Term + dead-letter log
//     (mirrors SQS maxReceiveCount -> DLQ; the work-queue stream has no DLQ
//     subject, so termination + the error log is the dead-letter path)
//   - NumDelivered > MaxDeliver -> Term (config-drift safety net)
//   - undecodable payload -> Term + error log (poison message)
//
// Messages in a batch are processed concurrently (like the Rust sqs_worker
// spawning one task per message and joining them): every in-flight message
// gets its own InProgress heartbeat, so a slow handler can no longer stall
// its batch-mates past the 30s AckWait and cause duplicate deliveries.
func Consume(ctx context.Context, js jetstream.JetStream, stream, durable, filterSubject string, handler Handler) error {
	// The stream's subject space is the stream name used as a subject prefix
	// (e.g. stream "jobs" owns "jobs.>").
	if _, err := natsx.EnsureStream(ctx, js, stream, []string{stream + ".>"}); err != nil {
		return fmt.Errorf("ensure stream %q: %w", stream, err)
	}
	s, err := js.Stream(ctx, stream)
	if err != nil {
		return fmt.Errorf("lookup stream %q: %w", stream, err)
	}
	consumer, err := natsx.EnsureConsumer(ctx, s, durable, filterSubject)
	if err != nil {
		return fmt.Errorf("ensure consumer %q on %q: %w", durable, stream, err)
	}

	log := slog.With("stream", stream, "durable", durable, "filter", filterSubject)
	log.Info("consumer started")

	for {
		if ctx.Err() != nil {
			log.Info("consumer stopping")
			return nil
		}

		batch, err := consumer.Fetch(fetchBatchSize, jetstream.FetchMaxWait(fetchMaxWait))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("fetch failed, retrying", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}

		// Handle the whole batch concurrently: each message acks/naks itself
		// when done, and each heartbeats independently while its handler runs.
		var wg sync.WaitGroup
		for msg := range batch.Messages() {
			wg.Add(1)
			go func(msg jetstream.Msg) {
				defer wg.Done()
				handleMessage(ctx, msg, handler, log)
			}(msg)
		}
		wg.Wait()

		if err := batch.Error(); err != nil && !errors.Is(err, context.Canceled) {
			// Timeouts/no-messages are normal for an idle consumer; anything
			// else is worth logging but the loop retries regardless.
			if !errors.Is(err, jetstream.ErrNoMessages) {
				log.Debug("fetch batch ended with error", "err", err)
			}
		}
	}
}

func handleMessage(ctx context.Context, msg jetstream.Msg, handler Handler, log *slog.Logger) {
	meta, _ := msg.Metadata()
	var deliveries uint64
	if meta != nil {
		deliveries = meta.NumDelivered
	}
	msgLog := log.With("subject", msg.Subject(), "deliveries", deliveries)

	// The server delivers at most MaxDeliver times, so this should never fire;
	// it guards config drift (e.g. MaxDeliver raised after deliveries already
	// exceeded the old value).
	if deliveries > maxDeliver {
		msgLog.Error("dead-letter: message exceeded MaxDeliver, terminating",
			"data", string(msg.Data()))
		if err := msg.Term(); err != nil {
			msgLog.Error("failed to term dead-lettered message", "err", err)
		}
		return
	}

	var env events.Envelope
	if err := json.Unmarshal(msg.Data(), &env); err != nil {
		msgLog.Error("poison message: undecodable envelope, terminating",
			"err", err, "data", string(msg.Data()))
		if err := msg.Term(); err != nil {
			msgLog.Error("failed to term poison message", "err", err)
		}
		return
	}
	msgLog = msgLog.With("event_id", env.ID, "type", env.Type)

	// Keep the delivery alive while the handler runs so slow handlers
	// (ffmpeg, large fetches) are not redelivered mid-processing.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(inProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := msg.InProgress(); err != nil {
					msgLog.Debug("in-progress ack failed", "err", err)
				}
			}
		}
	}()

	if err := handler(ctx, env); err != nil {
		if deliveries >= maxDeliver {
			// Last allowed delivery: the server will not redeliver, so Term
			// drops the message from the work-queue stream rather than
			// leaving it un-acked forever. This is the dead-letter path —
			// the error log above is the record (no DLQ subject exists).
			msgLog.Error("dead-letter: handler failed on final delivery, terminating",
				"err", err, "data", string(msg.Data()))
			if terr := msg.Term(); terr != nil {
				msgLog.Error("failed to term dead-lettered message", "err", terr)
			}
			return
		}
		delay := nakDelay(deliveries)
		msgLog.Warn("handler failed, nacking for redelivery",
			"err", err, "redeliver_in", delay)
		if err := msg.NakWithDelay(delay); err != nil {
			msgLog.Error("failed to nak message", "err", err)
		}
		return
	}

	if err := msg.Ack(); err != nil {
		msgLog.Error("failed to ack message", "err", err)
	}
}

// nakDelay is the exponential redelivery backoff for a failed handler:
// base * 2^(deliveries-1), capped. deliveries is >= 1 for any message the
// server has handed out at least once.
func nakDelay(deliveries uint64) time.Duration {
	delay := nakBackoffBase
	for i := uint64(1); i < deliveries && delay < nakBackoffMax; i++ {
		delay *= 2
	}
	return min(delay, nakBackoffMax)
}
