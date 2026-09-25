-- Queries ported from crates/notification_db_client/src/user_notification/

-- name: CreateBulkUserNotifications :exec
INSERT INTO user_notification (notification_id, user_id)
SELECT $1, user_id
FROM UNNEST(sqlc.arg(user_ids)::text[]) AS user_id;

-- name: DeleteAllUserNotifications :exec
DELETE FROM user_notification
WHERE user_id = $1;

-- name: DeleteUserNotification :exec
-- Soft delete.
UPDATE user_notification
SET deleted_at = now()
WHERE user_id = $1 AND notification_id = $2;

-- name: BulkDeleteUserNotification :exec
-- Soft delete.
UPDATE user_notification
SET deleted_at = now()
WHERE user_id = $1
AND notification_id = ANY(sqlc.arg(notification_ids)::uuid[]);

-- name: PatchDone :exec
UPDATE user_notification
SET state = 'done'::notification_state
WHERE notification_id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- name: BulkPatchDone :exec
-- done=true marks done; done=false un-dones back to 'seen' (leaves other
-- states untouched).
UPDATE user_notification
SET state = CASE
    WHEN sqlc.arg(done)::bool THEN 'done'::notification_state
    WHEN state = 'done' THEN 'seen'::notification_state
    ELSE state
END
WHERE user_id = $1
AND notification_id = ANY(sqlc.arg(notification_ids)::uuid[])
AND deleted_at IS NULL;

-- name: PatchSeen :exec
UPDATE user_notification
SET state = CASE WHEN state = 'unseen' THEN 'seen'::notification_state ELSE state END,
    seen_at = COALESCE(seen_at, NOW())
WHERE notification_id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- name: BulkPatchSeen :exec
UPDATE user_notification un
SET state = CASE WHEN state = 'unseen' THEN 'seen'::notification_state ELSE state END,
    seen_at = COALESCE(seen_at, NOW())
WHERE un.user_id = $1
AND un.notification_id = ANY(sqlc.arg(notification_ids)::uuid[])
AND un.deleted_at IS NULL;

-- name: BulkPatchSent :exec
UPDATE user_notification
SET sent = true
WHERE notification_id = $1 AND user_id = ANY(sqlc.arg(user_ids)::text[]);

-- name: BulkPatchSentByNotificationEventItemIDs :exec
UPDATE user_notification
SET sent = true
FROM notification n
WHERE n.id = user_notification.notification_id
AND n.event_item_id = ANY(sqlc.arg(notification_event_item_ids)::text[])
AND user_notification.user_id = $1;
