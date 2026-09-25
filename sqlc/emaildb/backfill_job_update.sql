-- name: UpdateBackfillJobStatus :exec
UPDATE email_backfill_jobs
        SET status = $1::email_backfill_job_status, updated_at = now()
        WHERE id = $2;


-- name: ClaimInitLease :one
UPDATE email_backfill_jobs
        SET init_lease_token = $2,
            init_lease_expires_at = now() + interval '2 minutes',
            updated_at = now()
        WHERE id = $1
          AND status = 'Init'
          AND initialized_at IS NULL
          AND (
              init_lease_expires_at IS NULL
              OR init_lease_expires_at < now()
          )
        RETURNING init_lease_token AS "init_lease_token";


-- name: ClaimInitLease2 :one
SELECT status::text AS "status", initialized_at
        FROM email_backfill_jobs
        WHERE id = $1;


-- name: RenewInitLease :exec
UPDATE email_backfill_jobs
        SET init_lease_expires_at = now() + interval '2 minutes',
            updated_at = now()
        WHERE id = $1
          AND status = 'Init'
          AND init_lease_token = $2
          AND init_lease_expires_at > now();


-- name: ReleaseInitLease :exec
UPDATE email_backfill_jobs
        SET init_lease_token = NULL,
            init_lease_expires_at = NULL,
            updated_at = now()
        WHERE id = $1
          AND status = 'Init'
          AND init_lease_token = $2;


-- name: FinalizeInitialization :exec
INSERT INTO email_backfill_init_outbox (id, backfill_job_id)
        VALUES ($1, $2)
        ON CONFLICT (backfill_job_id) DO NOTHING;


-- name: FinalizeInitialization2 :exec
UPDATE email_backfill_jobs
        SET status = 'InProgress',
            initialized_at = now(),
            init_lease_token = NULL,
            init_lease_expires_at = NULL,
            updated_at = now()
        WHERE id = $1
          AND status = 'Init'
          AND init_lease_token = $2
          AND init_lease_expires_at > now();


-- name: FailBackfillJob :exec
UPDATE email_backfill_jobs
        SET status = 'Failed',
            updated_at = now()
        WHERE id = $1
          AND status IN ('Init', 'InProgress');


-- name: CompleteBackfillJob :exec
UPDATE email_backfill_jobs
        SET status = 'Complete',
            initialized_at = COALESCE(initialized_at, now()),
            init_lease_token = NULL,
            init_lease_expires_at = NULL,
            updated_at = now()
        WHERE id = $1
          AND (
              (
                  status = 'Init'
                  AND $2::uuid IS NOT NULL
                  AND init_lease_token = $2
                  AND init_lease_expires_at > now()
              )
              OR (
                  status = 'InProgress'
                  AND $2::uuid IS NULL
              )
          );


-- name: CompleteBackfillJob2 :one
SELECT status::text AS "status"
            FROM email_backfill_jobs
            WHERE id = $1;


-- name: CompleteBackfillJob3 :exec
INSERT INTO email_backfill_completion_outbox (id, backfill_job_id)
        VALUES ($1, $2)
        ON CONFLICT (backfill_job_id) DO NOTHING;


-- name: CompletionEffectsPending :one
SELECT (completed_at IS NULL)::bool AS "pending"
        FROM email_backfill_completion_outbox
        WHERE backfill_job_id = $1;


-- name: ClaimCompletionEffects :one
UPDATE email_backfill_completion_outbox
        SET effects_lease_token = $2,
            effects_lease_expires_at = now() + interval '2 minutes'
        WHERE backfill_job_id = $1
          AND completed_at IS NULL
          AND (
              effects_lease_token IS NULL
              OR effects_lease_expires_at < now()
          )
        RETURNING effects_lease_token AS "effects_lease_token";


-- name: RenewCompletionEffects :exec
UPDATE email_backfill_completion_outbox
        SET effects_lease_expires_at = now() + interval '2 minutes'
        WHERE backfill_job_id = $1
          AND completed_at IS NULL
          AND effects_lease_token = $2
          AND effects_lease_expires_at > now();


-- name: ReleaseCompletionEffects :exec
UPDATE email_backfill_completion_outbox
        SET effects_lease_token = NULL,
            effects_lease_expires_at = NULL
        WHERE backfill_job_id = $1
          AND completed_at IS NULL
          AND effects_lease_token = $2;


-- name: MarkCompletionEffectsComplete :exec
UPDATE email_backfill_completion_outbox
        SET completed_at = now()
        WHERE backfill_job_id = $1
          AND completed_at IS NULL
          AND effects_lease_token = $2
          AND effects_lease_expires_at > now();


-- name: CancelActiveJobsByLinkId :exec
UPDATE email_backfill_jobs
        SET status = $1::email_backfill_job_status, updated_at = now()
        WHERE link_id = $2
        AND status IN ('Init', 'InProgress');


-- name: UpdateJobTotalThreads :exec
UPDATE email_backfill_jobs
        SET total_threads = $1, updated_at = now()
        WHERE id = $2;


-- name: UpdateJobThreadsRetrievedCount :exec
UPDATE email_backfill_jobs
        SET threads_retrieved_count = threads_retrieved_count + $1, updated_at = now()
        WHERE id = $2;

