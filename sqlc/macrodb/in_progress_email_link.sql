-- name: CheckExistingInProgressEmailLink :one
SELECT
                macro_user_id,
                id
            FROM
                in_progress_email_link
            WHERE
                in_progress_email_link.email = $1;


-- name: InsertInProgressEmailLink :exec
INSERT INTO
                in_progress_email_link (id, macro_user_id, email, created_at)
            VALUES
                ($1, $2, $3, NOW());


-- name: GetInProgressEmailLink :one
SELECT
                id,
                macro_user_id,
                email
            FROM
                in_progress_email_link
            WHERE
                id = $1;


-- name: DeleteInProgressEmailLink :exec
DELETE FROM
                in_progress_email_link
            WHERE
                id = $1;


-- name: DeleteDayOldInProgressEmailLinks :exec
DELETE FROM
                in_progress_email_link
            WHERE
                created_at < $1;

