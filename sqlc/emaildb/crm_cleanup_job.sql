-- name: CreateJob :one
INSERT INTO crm_cleanup_jobs (id, total_candidates, max_candidate_id, status)
        VALUES ($1, $2, $3, 'Init')
        ON CONFLICT ((TRUE)) WHERE status IN ('Init', 'InProgress') DO NOTHING
        RETURNING
            id,
            status as "status",
            total_candidates,
            dispatched_count,
            max_candidate_id,
            created_at,
            updated_at;


-- name: GetActiveJob :one
SELECT
            id,
            status as "status",
            total_candidates,
            dispatched_count,
            max_candidate_id,
            created_at,
            updated_at
        FROM crm_cleanup_jobs
        WHERE status IN ('Init', 'InProgress');


-- name: GetJob :one
SELECT
            id,
            status as "status",
            total_candidates,
            dispatched_count,
            max_candidate_id,
            created_at,
            updated_at
        FROM crm_cleanup_jobs
        WHERE id = $1;


-- name: AddDispatchedCount :exec
UPDATE crm_cleanup_jobs
        SET dispatched_count = dispatched_count + $1, updated_at = now()
        WHERE id = $2;


-- name: SetJobStatus :exec
UPDATE crm_cleanup_jobs
        SET status = $1::crm_cleanup_job_status, updated_at = now()
        WHERE id = $2;

