// Postgres storage + the polling aggregator, porting
// frecency::outbound::postgres (FrecencyPgStorage, FrecencyPgProcessor) and
// frecency::inbound::polling_aggregator.
//
// The aggregator claims unprocessed events under a session advisory lock —
// the same correctness rule as the Rust processor: aggregation is a
// read-modify-write of frecency_aggregates keyed by
// (user_id, entity_type, entity_id), so two concurrent pollers would
// blind-overwrite each other's contribution. At-most-once delivery is
// deliberate (a lost batch only degrades ordering slightly).
package frecency

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lockID mirrors FRECENCY_POLLER_LOCK_ID in the Rust processor.
const lockID int64 = 999_999_001

// fetchLimit mirrors FETCH_LIMIT (u16::MAX / 7 params per event row).
const fetchLimit = 65535 / 7

// DefaultPollInterval matches the connection_gateway worker cadence.
const DefaultPollInterval = 60 * time.Second

// Store is the outbound Postgres adapter for frecency tables.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// RecordEvent appends one event to frecency_events (ports
// EventRecordStorage::set_event; connection_id is kept for compatibility
// but no longer used).
func (s *Store) RecordEvent(ctx context.Context, userID, entityType, entityID, action, connID string, ts time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO frecency_events
			(user_id, entity_type, event_type, timestamp, connection_id, entity_id, was_processed)
		VALUES ($1, $2, $3, $4, $5, $6, false)`,
		userID, entityType, action, ts, connID, entityID)
	return err
}

// AggregatesFor returns frecency scores for a user's entities, keyed by
// entity_id (ports get_aggregate_for_user_entities, channel-scoped callers
// only need the score).
func (s *Store) AggregatesFor(ctx context.Context, userID, entityType string, entityIDs []string) (map[string]Aggregate, error) {
	out := map[string]Aggregate{}
	if len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT entity_id, event_count, frecency_score, first_event, recent_events
		FROM frecency_aggregates
		WHERE user_id = $1 AND entity_type = $2 AND entity_id = ANY($3)`,
		userID, entityType, entityIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Aggregate
		var raw []byte
		if err := rows.Scan(&a.EntityID, &a.EventCount, &a.Score, &a.FirstEvent, &raw); err != nil {
			return nil, err
		}
		a.UserID, a.EntityType = userID, entityType
		if err := json.Unmarshal(raw, &a.RecentEvent); err != nil {
			return nil, err
		}
		out[a.EntityID] = a
	}
	return out, rows.Err()
}

var errLocked = errors.New("another poller holds the frecency aggregation lock")

// RunAggregator is the polling worker: every interval it claims unprocessed
// events and folds them into frecency_aggregates. Blocks until ctx ends.
func (s *Store) RunAggregator(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := s.ProcessBatch(ctx)
		switch {
		case errors.Is(err, errLocked):
			// Another replica is mid-pass; normal under multiple instances.
		case err != nil:
			slog.Warn("frecency: aggregation pass failed", "err", err)
		default:
			if n > 0 {
				slog.Debug("frecency: aggregated events", "events", n)
			}
		}
	}
}

// ProcessBatch runs one aggregation pass; returns the number of events
// folded. ErrLocked is returned when another poller holds the lock.
func (s *Store) ProcessBatch(ctx context.Context) (int, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockID).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, errLocked
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID)
	}()

	// Claim a batch up front (ports the FOR UPDATE SKIP LOCKED claim).
	// RETURNING has no ORDER BY — wrap in a CTE and sort the result so
	// same-key events fold in insertion order (recent_events is push-front).
	rows, err := conn.Query(ctx, `
		WITH claimed AS (
			UPDATE frecency_events SET was_processed = true
			WHERE id IN (
				SELECT id FROM frecency_events
				WHERE was_processed = false
				ORDER BY id
				LIMIT $1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, user_id, entity_type, event_type, timestamp, entity_id
		)
		SELECT * FROM claimed ORDER BY id`, fetchLimit)
	if err != nil {
		return 0, err
	}
	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.UserID, &e.EntityType, &e.Action, &e.Timestamp, &e.EntityID); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil // idle pass — short-circuit like the Rust service
	}

	// Group by aggregate key preserving event order.
	type key struct{ user, etype, eid string }
	order := []key{}
	byKey := map[key][]Event{}
	for _, e := range events {
		k := key{e.UserID, e.EntityType, e.EntityID}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], e)
	}

	// Load existing aggregates for the involved keys.
	users, etypes, eids := make([]string, 0, len(order)), make([]string, 0, len(order)), make([]string, 0, len(order))
	for _, k := range order {
		users = append(users, k.user)
		etypes = append(etypes, k.etype)
		eids = append(eids, k.eid)
	}
	existing := map[key]*Aggregate{}
	arows, err := conn.Query(ctx, `
		SELECT a.user_id, a.entity_type, a.entity_id, a.event_count,
		       a.frecency_score, a.first_event, a.recent_events
		FROM frecency_aggregates a
		JOIN unnest($1::text[], $2::text[], $3::text[]) AS t(u, t, e)
		  ON a.user_id = t.u AND a.entity_type = t.t AND a.entity_id = t.e`,
		users, etypes, eids)
	if err != nil {
		return 0, err
	}
	for arows.Next() {
		var a Aggregate
		var raw []byte
		if err := arows.Scan(&a.UserID, &a.EntityType, &a.EntityID, &a.EventCount,
			&a.Score, &a.FirstEvent, &raw); err != nil {
			arows.Close()
			return 0, err
		}
		if err := json.Unmarshal(raw, &a.RecentEvent); err != nil {
			arows.Close()
			return 0, err
		}
		agg := a
		existing[key{a.UserID, a.EntityType, a.EntityID}] = &agg
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return 0, err
	}

	// Fold events (ports the SyncWorker merge): first event creates the
	// aggregate via New, the rest append.
	now := time.Now().UTC()
	merged := make([]*Aggregate, 0, len(order))
	for _, k := range order {
		a, ok := existing[k]
		for i, e := range byKey[k] {
			if !ok && i == 0 {
				na := NewAggregate(k.user, k.etype, k.eid, e, now)
				a = &na
				continue
			}
			a.Append(e, now)
		}
		if a != nil {
			merged = append(merged, a)
		}
	}

	// Upsert aggregates.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	for _, a := range merged {
		raw, err := json.Marshal(a.RecentEvent)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO frecency_aggregates
				(entity_id, entity_type, user_id, event_count, frecency_score, first_event, recent_events)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (user_id, entity_type, entity_id) DO UPDATE SET
				event_count = EXCLUDED.event_count,
				frecency_score = EXCLUDED.frecency_score,
				recent_events = EXCLUDED.recent_events`,
			a.EntityID, a.EntityType, a.UserID, a.EventCount, a.Score, a.FirstEvent, raw); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(events), nil
}
