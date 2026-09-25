-- name: DeleteThreadIfEmpty :one
SELECT EXISTS(SELECT 1 FROM email_messages WHERE thread_id = $1) AS "exists";


-- name: DeleteThreadIfEmpty2 :exec
DELETE FROM email_threads WHERE id = $1;

