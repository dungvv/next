// Package contacts ports crates/contacts + services/contacts_service:
// contacts CRUD against macrodb, a NATS notifier replacing the connection
// gateway HTTP call, a JetStream consumer replacing the SQS ingress worker,
// and the backfill outbox poller.
package contacts

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the ContactsRepository port.
type Repository interface {
	// GetContacts returns contact user ids for userID (excluding self).
	GetContacts(ctx context.Context, userID string) ([]string, error)
	// CreateConnections upserts undirected connection pairs.
	CreateConnections(ctx context.Context, connections [][2]string) error
	// GetUnappliedOutbox returns pending contacts_backfill_outbox rows.
	GetUnappliedOutbox(ctx context.Context) ([]OutboxMessage, error)
	// MarkOutboxApplied sets applied_at on an outbox row.
	MarkOutboxApplied(ctx context.Context, id int64) error
}

// OutboxMessage mirrors ContactsBackfillOutboxMessage.
type OutboxMessage struct {
	ID                  int64
	ChannelID           uuid.UUID
	ChannelParticipants []string
}

// PgRepository is the macrodb implementation (ports DbContactsRepository).
type PgRepository struct {
	db *pgxpool.Pool
}

func NewPgRepository(db *pgxpool.Pool) *PgRepository { return &PgRepository{db: db} }

func (r *PgRepository) GetContacts(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.db.Query(ctx, `
		SELECT user1 AS contact FROM contacts_connections WHERE user2 = $1
		UNION
		SELECT user2 AS contact FROM contacts_connections WHERE user1 = $1`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("get contacts: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *PgRepository) CreateConnections(ctx context.Context, connections [][2]string) error {
	// Contacts are undirected: drop self-edges and canonicalize endpoint order
	// (user1 < user2, matching the CHECK constraint) before hitting Postgres.
	type pair struct{ a, b string }
	seen := map[pair]struct{}{}
	var u1, u2 []string
	for _, c := range connections {
		a, b := c[0], c[1]
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		p := pair{a, b}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		u1 = append(u1, a)
		u2 = append(u2, b)
	}
	if len(u1) == 0 {
		return nil
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO contacts_connections(user1, user2)
		SELECT * FROM unnest($1::text[], $2::text[])
		ON CONFLICT(user1, user2) DO UPDATE SET updated_at = now()`,
		u1, u2)
	if err != nil {
		return fmt.Errorf("create connections: %w", err)
	}
	return nil
}

func (r *PgRepository) GetUnappliedOutbox(ctx context.Context) ([]OutboxMessage, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, comms_channel_id, user_ids
		FROM contacts_backfill_outbox
		WHERE applied_at IS NULL
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("get unapplied outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		var raw []byte
		if err := rows.Scan(&m.ID, &m.ChannelID, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &m.ChannelParticipants); err != nil {
			return nil, fmt.Errorf("decode outbox user_ids: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *PgRepository) MarkOutboxApplied(ctx context.Context, id int64) error {
	_, err := r.db.Exec(ctx,
		`UPDATE contacts_backfill_outbox SET applied_at = now() WHERE id = $1`, id)
	return err
}
