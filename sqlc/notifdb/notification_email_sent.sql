-- Queries ported from crates/notification_db_client/src/notification_email_sent/

-- name: CreateNotificationEmailSent :exec
INSERT INTO notification_email_sent (user_id)
VALUES ($1);

-- name: DeleteNotificationEmailSent :exec
DELETE FROM notification_email_sent
WHERE user_id = $1;

-- name: GetNotificationEmailSentBulk :many
SELECT user_id
FROM notification_email_sent
WHERE user_id = ANY(sqlc.arg(user_ids)::text[]);
