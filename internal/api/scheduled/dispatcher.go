package scheduled

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
)

const (
	// SubjectScheduledDue is the jobs-stream subject carrying "action is due"
	// wake-up messages (replaces the Rust polling loop's scheduling trigger).
	SubjectScheduledDue = "jobs.scheduled_due"

	// TickInterval is the fallback poll cadence; it also feeds the
	// jobs.scheduled_due publisher so multi-binary deploys get pushed wake-ups.
	TickInterval = 30 * time.Second
)

// Dispatcher owns due-action discovery: a 30s Postgres ticker (authoritative
// for single-binary runs) plus a JetStream consumer on jobs.scheduled_due so
// other instances can push wake-ups.
type Dispatcher struct {
	repo     *Repo
	executor Executor
	js       jetstream.JetStream
	nc       *nats.Conn
}

// NewDispatcher builds the dispatcher; js/nc may be nil to disable NATS paths.
func NewDispatcher(repo *Repo, executor Executor, js jetstream.JetStream, nc *nats.Conn) *Dispatcher {
	return &Dispatcher{repo: repo, executor: executor, js: js, nc: nc}
}

// Run starts the ticker and the JetStream consumer; blocks until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) error {
	if err := d.EnsureConsumer(ctx); err != nil {
		slog.Error("scheduled: jobs consumer unavailable", "err", err)
	}

	go d.ticker(ctx)
	if cons, err := d.consumer(ctx); err == nil {
		go d.consume(ctx, cons)
	} else if d.js != nil {
		slog.Error("scheduled: no JetStream consumer, ticker only", "err", err)
	}

	<-ctx.Done()
	return nil
}

// EnsureConsumer creates the jobs stream + durable due-actions consumer.
func (d *Dispatcher) EnsureConsumer(ctx context.Context) error {
	if d.js == nil {
		return nil
	}
	stream, err := natsx.EnsureStream(ctx, d.js, events.StreamJobs,
		[]string{events.StreamJobs + ".>"})
	if err != nil {
		return err
	}
	_, err = natsx.EnsureConsumer(ctx, stream, "scheduled-due", SubjectScheduledDue)
	return err
}

// PublishDue enqueues a wake-up for an action whose next_run_at arrived.
func (d *Dispatcher) PublishDue(ctx context.Context, actionID uuid.UUID) error {
	if d.js == nil {
		return nil
	}
	env, err := events.New("scheduled.due", "scheduled", actionID.String(), 1,
		map[string]string{"action_id": actionID.String()})
	if err != nil {
		return err
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = d.js.Publish(ctx, SubjectScheduledDue, data)
	return err
}

// ticker scans Postgres every TickInterval; due unclaimed rows are executed
// directly and a jobs.scheduled_due message is published for other instances.
func (d *Dispatcher) ticker(ctx context.Context) {
	t := time.NewTicker(TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.scanDue(ctx)
		}
	}
}

func (d *Dispatcher) scanDue(ctx context.Context) {
	actions, err := d.repo.GetNextUnclaimedActions(ctx, 10)
	if err != nil {
		slog.Error("scheduled: due-scan failed", "err", err)
		return
	}
	now := time.Now().UTC()
	for _, a := range actions {
		if a.NextRunAt.After(now) {
			continue
		}
		// Push a wake-up so horizontally-scaled instances share the work;
		// the claim check in ExecuteAction dedupes whoever wins.
		if err := d.PublishDue(ctx, a.ID); err != nil {
			slog.Error("scheduled: failed to publish due msg", "action_id", a.ID, "err", err)
		}
		go func(a ScheduledAction) {
			if _, err := d.executor.ExecuteAction(ctx, a); err != nil {
				var already *AlreadyRunningError
				if errors.As(err, &already) {
					return
				}
				slog.Error("scheduled: execute failed", "action_id", a.ID, "err", err)
			}
		}(a)
	}
}

// consumer returns the JetStream durable bound to jobs.scheduled_due.
func (d *Dispatcher) consumer(ctx context.Context) (jetstream.Consumer, error) {
	stream, err := d.js.Stream(ctx, events.StreamJobs)
	if err != nil {
		return nil, err
	}
	return stream.Consumer(ctx, "scheduled-due")
}

// consume drains jobs.scheduled_due; each message re-scans due rows so the
// exact action set is always recomputed from Postgres (claims dedupe).
func (d *Dispatcher) consume(ctx context.Context, cons jetstream.Consumer) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msgs, err := cons.Fetch(10, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("scheduled: fetch failed", "err", err)
			time.Sleep(time.Second)
			continue
		}
		for msg := range msgs.Messages() {
			d.scanDue(ctx)
			if err := msg.Ack(); err != nil {
				slog.Error("scheduled: ack failed", "err", err)
			}
		}
		if msgs.Error() != nil {
			slog.Error("scheduled: fetch error", "err", msgs.Error())
		}
	}
}
