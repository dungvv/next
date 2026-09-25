-- Queries ported from crates/notification_db_client/src/user_mute_notification.rs

-- name: RemoveUserMuteNotification :exec
DELETE FROM user_mute_notification WHERE user_id = $1;

-- name: UpsertUserMuteNotification :exec
INSERT INTO user_mute_notification (user_id) VALUES ($1)
ON CONFLICT (user_id) DO NOTHING;

-- name: GetUserMuteNotificationBulk :many
SELECT user_id FROM user_mute_notification
WHERE user_id = ANY(sqlc.arg(user_ids)::text[]);
