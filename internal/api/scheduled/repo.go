package scheduled

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo is the ScheduledActionRepo port: Postgres persistence for actions and
// their execution records (outbound/pg_scheduled_action_repo.rs).
type Repo struct {
	pool *pgxpool.Pool
}

// NewRepo builds the macrodb-backed repository.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const actionColumns = `id, owner, name, schedule, kind, timezone, task, claimed, created_at, updated_at, next_run_at, enabled`

func scanAction(row interface{ Scan(...any) error }) (ScheduledAction, error) {
	var a ScheduledAction
	var kind string
	err := row.Scan(
		&a.ID, &a.Owner, &a.Name, &a.Schedule, &kind, &a.Timezone,
		&a.Task, &a.Claimed, &a.CreatedAt, &a.UpdatedAt, &a.NextRunAt, &a.Enabled,
	)
	if err != nil {
		return ScheduledAction{}, err
	}
	a.Kind = ActionKind(kind)
	return a, nil
}

// CreateAction inserts the action and registers it in the shared `entity`
// table (entity_registry insert_entity), in one transaction.
func (r *Repo) CreateAction(ctx context.Context, a ScheduledAction) (ScheduledAction, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ScheduledAction{}, err
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx,
		`INSERT INTO scheduled_action (owner, name, schedule, kind, timezone, task, next_run_at, enabled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING `+actionColumns,
		a.Owner, a.Name, a.Schedule, string(a.Kind), a.Timezone, a.Task, a.NextRunAt, a.Enabled)
	created, err := scanAction(row)
	if err != nil {
		return ScheduledAction{}, err
	}

	// insert_entity / NewEntityRecord(ScheduledAction): idempotent upsert.
	if _, err := tx.Exec(ctx,
		`INSERT INTO entity (id, entity_type, owner_type, owner_id)
		 VALUES ($1, 'scheduled_action', 'user', $2)
		 ON CONFLICT (id) DO NOTHING`,
		created.ID, created.Owner); err != nil {
		return ScheduledAction{}, fmt.Errorf("entity registry insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ScheduledAction{}, err
	}
	return created, nil
}

// GetActions returns every action owned by the user.
func (r *Repo) GetActions(ctx context.Context, userID string) ([]ScheduledAction, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+actionColumns+` FROM scheduled_action WHERE owner = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledAction
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetNextUnclaimedActions returns the next `limit` enabled actions ordered by
// next_run_at, skipping rows claimed within MaxActionTime.
func (r *Repo) GetNextUnclaimedActions(ctx context.Context, limit int) ([]ScheduledAction, error) {
	stale := time.Now().UTC().Add(-MaxActionTime)
	rows, err := r.pool.Query(ctx,
		`SELECT `+actionColumns+` FROM scheduled_action
		 WHERE enabled AND (claimed IS NULL OR claimed < $1)
		 ORDER BY next_run_at ASC
		 LIMIT $2`,
		stale, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledAction
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAction rewrites the mutable fields and returns the stored row.
func (r *Repo) UpdateAction(ctx context.Context, a ScheduledAction) (ScheduledAction, error) {
	row := r.pool.QueryRow(ctx,
		`UPDATE scheduled_action
		 SET name = $1, schedule = $2, kind = $3, timezone = $4, task = $5,
		     next_run_at = $6, enabled = $7, updated_at = now()
		 WHERE id = $8
		 RETURNING `+actionColumns,
		a.Name, a.Schedule, string(a.Kind), a.Timezone, a.Task, a.NextRunAt, a.Enabled, a.ID)
	return scanAction(row)
}

// DeleteAction removes the action and its entity-registry row in one tx.
func (r *Repo) DeleteAction(ctx context.Context, id uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM scheduled_action WHERE id = $1`, id); err != nil {
		return err
	}
	// delete_entity: soft-delete the registry row.
	if _, err := tx.Exec(ctx,
		`UPDATE entity SET deleted_at = now(), updated_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("entity registry delete: %w", err)
	}
	return tx.Commit(ctx)
}

// ClaimAction sets `claimed` if the row is free or its claim is stale;
// returns *AlreadyRunningError otherwise.
func (r *Repo) ClaimAction(ctx context.Context, id uuid.UUID) error {
	now := time.Now().UTC()
	stale := now.Add(-MaxActionTime)
	tag, err := r.pool.Exec(ctx,
		`UPDATE scheduled_action
		 SET claimed = $1, updated_at = now()
		 WHERE id = $2 AND (claimed IS NULL OR claimed < $3)`,
		now, id, stale)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &AlreadyRunningError{ActionID: id}
	}
	return nil
}

// ReleaseAction clears the claim.
func (r *Repo) ReleaseAction(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE scheduled_action SET claimed = NULL, updated_at = now() WHERE id = $1`, id)
	return err
}

// CreateExecutionRecord appends a run record.
func (r *Repo) CreateExecutionRecord(ctx context.Context, rec ActionExecutionRecord) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO action_execution_record (action_id, resource_id, start_time, end_time, is_success, result)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.ActionID, rec.ResourceID, rec.StartTime, rec.EndTime, rec.IsSuccess, rec.Result)
	return err
}

// GetExecutionRecords lists run records newest-first.
func (r *Repo) GetExecutionRecords(ctx context.Context, actionID uuid.UUID) ([]ActionExecutionRecord, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, action_id, resource_id, start_time, end_time, is_success, result, created_at
		 FROM action_execution_record
		 WHERE action_id = $1
		 ORDER BY start_time DESC`, actionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionExecutionRecord
	for rows.Next() {
		var rec ActionExecutionRecord
		if err := rows.Scan(&rec.ID, &rec.ActionID, &rec.ResourceID, &rec.StartTime,
			&rec.EndTime, &rec.IsSuccess, &rec.Result, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// UpdateNextRunAt recomputes next_run_at from the stored cron + timezone.
func (r *Repo) UpdateNextRunAt(ctx context.Context, id uuid.UUID) error {
	var sched, tz string
	err := r.pool.QueryRow(ctx,
		`SELECT schedule, timezone FROM scheduled_action WHERE id = $1`, id).
		Scan(&sched, &tz)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	next, err := nextRunAfterNow(sched, tz, time.Now().UTC())
	if err != nil {
		// No future firings: leave next_run_at as-is, like the Rust repo.
		return nil
	}
	_, err = r.pool.Exec(ctx,
		`UPDATE scheduled_action SET next_run_at = $1, updated_at = now() WHERE id = $2`,
		next, id)
	return err
}

// UpdateLastExecuted stamps updated_at with the execution finish time.
func (r *Repo) UpdateLastExecuted(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE scheduled_action SET updated_at = $1 WHERE id = $2`,
		pgtype.Timestamptz{Time: at, Valid: true}, id)
	return err
}
