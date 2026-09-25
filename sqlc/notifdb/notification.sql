-- Queries ported from crates/notification_db_client/src/notification/
-- (mod.rs + delete.rs share delete_notification_by_event_item; get.rs has the
-- basic-notification fetch and collapse-key update.)

-- name: DeleteNotificationByEventItem :exec
DELETE FROM notification
WHERE event_item_id = $1 AND event_item_type = $2;

-- name: GetBasicNotification :one
SELECT
    n.event_item_id,
    n.event_item_type,
    n.notification_event_type,
    n.apns_collapse_key
FROM notification n
WHERE n.id = $1;

-- name: UpdateCollapseKey :one
UPDATE notification
SET apns_collapse_key = $2
WHERE id = $1
RETURNING
    event_item_id,
    event_item_type,
    notification_event_type,
    apns_collapse_key;
