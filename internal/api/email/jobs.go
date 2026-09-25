package email

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/macro-inc/macro/pkg/events"
)

// Job subjects on the `jobs` JetStream stream (Rust used SQS queues; the Go
// selfhost consolidates them onto the single stream with per-purpose
// subjects, matching internal/jobs conventions).
const (
	subjEmailRefresh  = "jobs.email_refresh"  // gmail webhook → inbox sync
	subjEmailBackfill = "jobs.email_backfill" // backfill job dispatch
	subjGmailOps      = "jobs.gmail_ops"      // gmail mutations (labels, block, ...)
	subjEmailSend     = "jobs.email_send"     // delayed scheduled send
	subjLinkManager   = "jobs.link_manager"   // link teardown
	subjEmailEvents   = "jobs.email_events"   // macro event stream (link_connected, ...)
)

// publishJob wraps payload in an events.Envelope and publishes it to the
// jobs stream. Returns an error when JetStream is unavailable.
func (d *Deps) publishJob(ctx context.Context, subject, eventType string, payload any) error {
	if d.JS == nil {
		return fmt.Errorf("jobs stream unavailable")
	}
	env, err := events.New(eventType, "email", subject, 1, payload)
	if err != nil {
		return fmt.Errorf("build envelope: %w", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if _, err := d.JS.Publish(ctx, subject, raw); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// publishJobBestEffort logs but does not fail on publish errors — mirrors the
// Rust "best-effort enqueue" behavior for gmail ops notifications.
func (d *Deps) publishJobBestEffort(ctx context.Context, subject, eventType string, payload any) {
	if err := d.publishJob(ctx, subject, eventType, payload); err != nil {
		slog.Warn("email: job enqueue failed", "subject", subject, "err", err)
	}
}

// --- payload shapes ----------------------------------------------------------

// RefreshInboxJob tells the inbox sync worker to fetch gmail history for a
// link starting at historyID.
type RefreshInboxJob struct {
	LinkID       uuid.UUID `json:"link_id"`
	EmailAddress string    `json:"email_address"`
	HistoryID    int64     `json:"history_id"`
}

// BackfillJobMsg dispatches a backfill job row to the backfill worker.
type BackfillJobMsg struct {
	JobID  uuid.UUID `json:"job_id"`
	LinkID uuid.UUID `json:"link_id"`
}

// GmailOpsJob mirrors sqs_client::gmail_ops::GmailOpsMessage variants. `kind`
// discriminates the payload.
type GmailOpsJob struct {
	Kind   string          `json:"kind"` // e.g. "modify_labels", "block_sender", "delete_label"
	LinkID uuid.UUID       `json:"link_id"`
	Data   json.RawMessage `json:"data"`
}

func gmailOps(linkID uuid.UUID, kind string, data any) GmailOpsJob {
	raw, _ := json.Marshal(data)
	return GmailOpsJob{Kind: kind, LinkID: linkID, Data: raw}
}

// OpsModifyLabels adds/removes provider label ids on a provider message.
type opsModifyLabels struct {
	ProviderMessageID string   `json:"provider_message_id"`
	AddLabels         []string `json:"add_labels,omitempty"`
	RemoveLabels      []string `json:"remove_labels,omitempty"`
}

// SendJobMsg dispatches a queued send (after the undo window).
type SendJobMsg struct {
	LinkID    uuid.UUID `json:"link_id"`
	MessageID uuid.UUID `json:"message_id"`
	ActorID   string    `json:"actor_id,omitempty"`
}

// LinkManagerDeleteJob asks the link manager to tear down a link's provider
// resources.
type LinkManagerDeleteJob struct {
	LinkID      uuid.UUID `json:"link_id"`
	Email       string    `json:"email"`
	AuthID      string    `json:"auth_id"`
	DeletedByID string    `json:"deleted_by_id"`
}
