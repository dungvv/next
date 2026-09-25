// Package email implements the Go port of services/email_service's HTTP API
// (mounted under /email) plus the gmail webhook receiver and the internal
// endpoints other services call.
//
// It reuses the generated pkg/store/emaildb queries, the pooled postgres
// connection, objectstore.Store (MinIO/S3) for attachment bytes, mail.Port
// (SMTP) for outbound sends, and NATS JetStream for async jobs
// (jobs.email_refresh, jobs.email_backfill, jobs.gmail_ops, ...).
//
// Provider-backed operations (Gmail API) go through the optional
// gmailx.Client; when Gmail credentials are not configured those features
// return 503 instead of silently failing.
package email

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/internal/api/email/gmailx"
	"github.com/macro-inc/macro/pkg/mail"
	"github.com/macro-inc/macro/pkg/objectstore"
	"github.com/macro-inc/macro/pkg/store/emaildb"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// Config holds the email-service-specific settings layered on top of the
// shared api.Config.
type Config struct {
	// AttachmentBucket is the S3/MinIO bucket for email attachment payloads
	// (draft uploads and cached provider attachments).
	AttachmentBucket string

	// Google OAuth credentials used to refresh Gmail access tokens.
	GoogleClientID     string
	GoogleClientSecret string

	// GmailPubsubTopic is the GCP Pub/Sub topic registered via users.watch
	// during /email/init (e.g. "projects/<proj>/topics/gmail").
	GmailPubsubTopic string

	// SendUndoDelaySecs is how long a send job waits before dispatch, giving
	// the user an undo window (mirrors send_undo_delay_secs in Rust config).
	SendUndoDelaySecs int

	// PresignGetSecs is the TTL for presigned attachment GET URLs.
	PresignGetSecs int
}

// Deps bundles every dependency the email router needs.
type Deps struct {
	Cfg    Config
	Pool   *pgxpool.Pool
	Q      *emaildb.Queries
	MacroQ *macrodb.Queries
	Store  objectstore.Store
	Mail   mail.Port // nil → send endpoints return 503
	NC     *nats.Conn
	JS     jetstream.JetStream // nil → queue-backed features degrade

	// Tokens stores Gmail OAuth2 refresh tokens per link; nil (or a missing
	// row) → provider-backed features return 503.
	Tokens *gmailx.TokenStore
}

func (d *Deps) q() *emaildb.Queries {
	if d.Q == nil {
		d.Q = emaildb.New(d.Pool)
	}
	return d.Q
}

func (d *Deps) mq() *macrodb.Queries {
	if d.MacroQ == nil {
		d.MacroQ = macrodb.New(d.Pool)
	}
	return d.MacroQ
}

// gmailClient returns a Gmail API client bound to the link, or nil when no
// Gmail credentials/tokens are configured.
func (d *Deps) gmailClient(ctx context.Context, linkID uuid.UUID) *gmailx.Client {
	if d.Tokens == nil {
		return nil
	}
	ok, err := d.Tokens.Has(ctx, linkID)
	if err != nil || !ok {
		return nil
	}
	return gmailx.New(d.Tokens.Source(linkID))
}
