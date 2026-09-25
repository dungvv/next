package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/macro-inc/macro/pkg/events"
)

// inProgressLinkMaxAge mirrors IN_PROGRESS_*_LINK_MAX_AGE in
// crates/macro_db_client: rows older than 24h are deleted.
const inProgressLinkMaxAge = 24 * time.Hour

// handleUserLinkCleanup ports services/user_link_cleanup_handler. Triggered
// by the scheduler (ex-EventBridge cron); the envelope payload is ignored.
func (d *deps) handleUserLinkCleanup(ctx context.Context, _ events.Envelope) error {
	cutoff := time.Now().UTC().Add(-inProgressLinkMaxAge)

	if _, err := d.pools.MacroDB.Exec(ctx, `
		DELETE FROM in_progress_user_link WHERE created_at < $1
	`, cutoff); err != nil {
		return fmt.Errorf("delete in_progress_user_link rows: %w", err)
	}

	if _, err := d.pools.MacroDB.Exec(ctx, `
		DELETE FROM in_progress_email_link WHERE created_at < $1
	`, cutoff); err != nil {
		return fmt.Errorf("delete in_progress_email_link rows: %w", err)
	}

	return nil
}
