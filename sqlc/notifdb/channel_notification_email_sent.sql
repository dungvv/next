-- Queries ported from crates/notification_db_client/src/channel_notification_email_sent/

-- name: DeleteChannelNotificationEmailSent :exec
DELETE FROM channel_notification_email_sent
WHERE channel_id = $1 AND user_id = $2;

-- name: DeleteChannelNotificationEmailSentByNotificationIDs :exec
DELETE FROM channel_notification_email_sent
USING notification n
WHERE n.id = ANY(sqlc.arg(notification_ids)::uuid[])
AND n.event_item_type = 'channel'
AND channel_notification_email_sent.channel_id = n.event_item_id::uuid
AND channel_notification_email_sent.user_id = sqlc.arg(user_id);

-- name: GetChannelNotificationEmailSentBulk :many
SELECT channel_id, user_id, created_at
FROM channel_notification_email_sent
WHERE channel_id = $1
AND user_id = ANY(sqlc.arg(user_ids)::text[]);

-- name: GetChannelNotificationEmailSentBulkByChannelIDs :many
SELECT channel_id
FROM channel_notification_email_sent
WHERE user_id = $1
AND channel_id = ANY(sqlc.arg(channel_ids)::uuid[]);

-- name: UpsertChannelNotificationEmailSentBulk :exec
INSERT INTO channel_notification_email_sent (channel_id, user_id)
SELECT $1, u FROM UNNEST(sqlc.arg(user_ids)::text[]) AS u
ON CONFLICT (channel_id, user_id) DO NOTHING;

-- name: UpsertChannelNotificationEmailSentBulkChannelIDs :exec
INSERT INTO channel_notification_email_sent (user_id, channel_id)
SELECT $1, c FROM UNNEST(sqlc.arg(channel_ids)::uuid[]) AS c
ON CONFLICT (channel_id, user_id) DO NOTHING;
