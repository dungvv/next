-- name: DeleteDbMessage :exec
DELETE FROM email_messages WHERE id = $1;

