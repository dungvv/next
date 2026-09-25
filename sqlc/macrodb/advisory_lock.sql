-- name: TryAcquireUserRefreshXactLock :one
SELECT pg_try_advisory_xact_lock($1);

