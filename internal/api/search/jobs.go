package search

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job status values (domain::jobs::JobStatus).
const (
	JobRunning   = "running"
	JobCompleted = "completed"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// jobsTable is the Postgres replacement for the DynamoDB backfill registry.
const jobsTable = "search_backfill_jobs"

// JobSnapshot mirrors domain::jobs::JobSnapshot on the wire.
type JobSnapshot struct {
	JobID      string     `json:"job_id"`
	Entity     string     `json:"entity"`
	Status     string     `json:"status"`
	Enqueued   int64      `json:"enqueued"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      *string    `json:"error,omitempty"`
}

// JobProgress is the per-page progress hook handed to the orchestrator —
// bumps the shared `enqueued` counter (DynamoDB `ADD` → SQL `+=`).
type JobProgress struct {
	jobs *BackfillJobs
	id   string
}

// Add records n more consumed rows. Best-effort like the DynamoDB backend.
func (p *JobProgress) Add(ctx context.Context, n int) {
	if n <= 0 {
		return
	}
	if _, err := p.jobs.db.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET enqueued = enqueued + $2 WHERE id = $1`, jobsTable),
		p.id, int64(n)); err != nil {
		slog.Warn("search: progress update failed", "job_id", p.id, "err", err)
	}
}

// JobHandle is what a spawned backfill needs: id for the client, a
// cancellable job context + progress hook for the orchestrator, and the
// cancel func the registry fires on shutdown.
type JobHandle struct {
	ID       string
	Progress *JobProgress
	Ctx      context.Context
	Cancel   context.CancelFunc
}

// BackfillJobs is the async-shareable job registry, backed by Postgres
// (DynamoDB replacement; expires_at is swept lazily). Cancellation tokens
// stay per-process like the Rust local_cancels map — the single binary has
// only one process anyway.
type BackfillJobs struct {
	db   *pgxpool.Pool
	ttl  time.Duration
	mu   sync.Mutex
	cans map[string]context.CancelFunc
}

// NewBackfillJobs builds the registry; ttl bounds snapshot retention.
func NewBackfillJobs(db *pgxpool.Pool, ttl time.Duration) *BackfillJobs {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &BackfillJobs{db: db, ttl: ttl, cans: map[string]context.CancelFunc{}}
}

// EnsureSchema creates the jobs table when missing.
// TODO(migrate): move this into crates/macro_db_client/migrations.
func (j *BackfillJobs) EnsureSchema(ctx context.Context) error {
	_, err := j.db.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id          TEXT PRIMARY KEY,
			entity      TEXT NOT NULL,
			status      TEXT NOT NULL,
			enqueued    BIGINT NOT NULL DEFAULT 0,
			started_at  TIMESTAMPTZ NOT NULL,
			finished_at TIMESTAMPTZ,
			error       TEXT,
			expires_at  TIMESTAMPTZ NOT NULL
		)`, jobsTable))
	return err
}

// Start allocates a job slot: writes the initial row and returns the handle.
// The job context is detached from the request ctx so the drain outlives
// the HTTP call.
func (j *BackfillJobs) Start(ctx context.Context, entity string) (*JobHandle, error) {
	id := uuid.NewString()
	// Opportunistic TTL sweep (DynamoDB TTL had no exact timing either).
	if _, err := j.db.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE expires_at < now()`, jobsTable)); err != nil {
		slog.Warn("search: job ttl sweep failed", "err", err)
	}
	_, err := j.db.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, entity, status, enqueued, started_at, expires_at)
		 VALUES ($1, $2, $3, 0, now(), now() + $4::interval)`, jobsTable),
		id, entity, JobRunning, j.ttl.String())
	if err != nil {
		return nil, fmt.Errorf("search: create job: %w", err)
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	j.mu.Lock()
	j.cans[id] = cancel
	j.mu.Unlock()
	return &JobHandle{
		ID:       id,
		Progress: &JobProgress{jobs: j, id: id},
		Ctx:      jobCtx,
		Cancel:   cancel,
	}, nil
}

// Finish records the terminal result; `cancelled` (job ctx fired before the
// drain returned) turns a nil error into "cancelled" like the Rust finish().
func (j *BackfillJobs) Finish(ctx context.Context, id string, cancelled bool, runErr error) {
	j.mu.Lock()
	delete(j.cans, id)
	j.mu.Unlock()

	status := JobCompleted
	var errStr *string
	switch {
	case runErr != nil:
		status = JobFailed
		s := runErr.Error()
		errStr = &s
	case cancelled:
		status = JobCancelled
	}
	if _, err := j.db.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET status = $2, finished_at = now(), error = $3 WHERE id = $1`, jobsTable),
		id, status, errStr); err != nil {
		slog.Warn("search: finish job failed", "job_id", id, "err", err)
	}
}

// Snapshot reads one job's current state; nil when expired or unknown.
func (j *BackfillJobs) Snapshot(ctx context.Context, id string) (*JobSnapshot, error) {
	var s JobSnapshot
	err := j.db.QueryRow(ctx, fmt.Sprintf(
		`SELECT id, entity, status, enqueued, started_at, finished_at, error
		 FROM %s WHERE id = $1 AND expires_at > now()`, jobsTable), id).
		Scan(&s.JobID, &s.Entity, &s.Status, &s.Enqueued, &s.StartedAt, &s.FinishedAt, &s.Error)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CancelAllLocal fires every locally tracked job cancel func — graceful
// shutdown stops drains between pages.
func (j *BackfillJobs) CancelAllLocal() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, cancel := range j.cans {
		cancel()
	}
}
