-- name: CheckUserEmailLink :one
SELECT id FROM email_links
    WHERE macro_id = $1;

