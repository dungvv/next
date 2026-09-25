-- Queries ported from crates/notification_db_client/src/unsubscribe/
-- Callers must lowercase emails before calling (Rust did it in-code).

-- name: IsEmailUnsubscribedBatch :many
SELECT t.email::text AS email,
       EXISTS(SELECT 1 FROM notification_email_unsubscribe u WHERE u.email = t.email) AS is_unsubscribed
FROM UNNEST(sqlc.arg(emails)::text[]) AS t(email);

-- name: UpsertEmailUnsubscribe :exec
INSERT INTO notification_email_unsubscribe (email) VALUES ($1)
ON CONFLICT (email) DO NOTHING;

-- name: RemoveEmailUnsubscribe :exec
DELETE FROM notification_email_unsubscribe WHERE email = $1;

-- name: GetUserUnsubscribes :many
SELECT
    unsubscribe_item.item_id,
    unsubscribe_item.item_type
FROM user_notification_item_unsubscribe unsubscribe_item
WHERE unsubscribe_item.user_id = $1;

-- name: GetUnsubscribedItemUsers :many
SELECT u.user_id
FROM user_notification_item_unsubscribe u
WHERE u.item_id = $1;

-- name: UpsertUnsubscribedItemUser :exec
INSERT INTO user_notification_item_unsubscribe (user_id, item_id, item_type)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, item_id) DO NOTHING;

-- name: RemoveUnsubscribedItemUser :exec
DELETE FROM user_notification_item_unsubscribe
WHERE user_id = $1 AND item_id = $2;
