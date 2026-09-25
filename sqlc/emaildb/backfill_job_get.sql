-- name: GetBackfillJob :one
SELECT
            id,
            link_id,
            fusionauth_user_id,
            threads_requested_limit,
            total_threads,
            threads_retrieved_count,
            status as "status",
            created_at,
            updated_at
        FROM email_backfill_jobs
        WHERE id = $1;


-- name: GetBackfillJobWithLinkId :one
SELECT
            id,
            link_id,
            fusionauth_user_id,
            threads_requested_limit,
            total_threads,
            threads_retrieved_count,
            status as "status",
            created_at,
            updated_at
        FROM email_backfill_jobs
        WHERE id = $1
        AND link_id = $2;


-- name: GetActiveBackfillJob :one
SELECT
            id,
            link_id,
            fusionauth_user_id,
            threads_requested_limit,
            total_threads,
            threads_retrieved_count,
            status as "status",
            created_at,
            updated_at
        FROM email_backfill_jobs
        WHERE link_id = $1 AND status IN ('Init', 'InProgress');


-- name: GetLatestBackfillJobByLinkId :one
SELECT
            id,
            link_id,
            fusionauth_user_id,
            threads_requested_limit,
            total_threads,
            threads_retrieved_count,
            status as "status",
            created_at,
            updated_at
        FROM email_backfill_jobs
        WHERE link_id = $1
        ORDER BY created_at DESC
        LIMIT 1;


-- name: GetAllJobsByFusionauthUserId :many
SELECT
            id,
            link_id,
            fusionauth_user_id,
            threads_requested_limit,
            total_threads,
            threads_retrieved_count,
            status as "status",
            created_at,
            updated_at
        FROM email_backfill_jobs
        WHERE fusionauth_user_id = $1
        ORDER BY created_at DESC
        LIMIT 100;

