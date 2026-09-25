package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/pkg/events"
)

// sfsDeleteMessage mirrors sqs_client::email::SFSDeleteMessage — one row of
// email_attachments_sfs that lost its attachment reference.
type sfsDeleteMessage struct {
	DBID  string `json:"db_id"`
	SFSID string `json:"sfs_id"`
}

// handleEmailSFSScan ports services/email_sfs_delete_handler: periodically
// (scheduler-triggered, ex-EventBridge cron(0 8 * * ? *)) find orphaned rows
// in email_attachments_sfs (attachment_id IS NULL) and enqueue one
// jobs.sfs_delete message each. The actual SFS delete is performed by the
// sfs_delete consumer (handleSFSDelete, ports email_service's sfs_deleter
// pubsub worker).
func (d *deps) handleEmailSFSScan(ctx context.Context, _ events.Envelope) error {
	rows, err := d.pools.MacroDB.Query(ctx, `
		SELECT id AS db_id, sfs_id
		FROM email_attachments_sfs
		WHERE attachment_id IS NULL
	`)
	if err != nil {
		return fmt.Errorf("query orphaned sfs attachments: %w", err)
	}
	messages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (sfsDeleteMessage, error) {
		var m sfsDeleteMessage
		var dbID, sfsID pgtype.UUID
		if err := row.Scan(&dbID, &sfsID); err != nil {
			return m, err
		}
		m.DBID, m.SFSID = dbID.String(), sfsID.String()
		return m, nil
	})
	if err != nil {
		return fmt.Errorf("scan orphaned sfs attachments: %w", err)
	}

	if len(messages) == 0 {
		slog.Info("email_sfs_delete: no orphaned sfs attachments found")
		return nil
	}
	slog.Info("email_sfs_delete: enqueueing sfs delete messages", "count", len(messages))

	for _, m := range messages {
		if err := d.publishJob(ctx, subjSFSDelete, "email.sfs_delete", m.SFSID, m); err != nil {
			slog.Error("email_sfs_delete: enqueue failed",
				"db_id", m.DBID, "sfs_id", m.SFSID, "err", err)
		}
	}
	return nil
}
