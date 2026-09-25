-- Queries ported from crates/comms_db_client/src/activity/

-- name: CreateActivity :one
INSERT INTO comms_activity (
    id,
    user_id,
    channel_id,
    created_at,
    updated_at
)
VALUES (
    $1, $2, $3, NOW(), NOW()
)
RETURNING
    id,
    user_id,
    channel_id,
    created_at,
    updated_at,
    viewed_at,
    interacted_at;

-- name: GetActivityForChannel :one
-- Rust used fetch_optional: callers must treat pgx.ErrNoRows as "no activity".
SELECT
    a.id,
    a.user_id,
    a.channel_id,
    a.viewed_at,
    a.interacted_at,
    a.created_at,
    a.updated_at
FROM comms_activity a
WHERE channel_id = $1 AND user_id = $2;

-- name: GetActivityForChannelBulk :many
SELECT
    a.user_id,
    a.updated_at
FROM comms_activity a
WHERE channel_id = $1 AND user_id = ANY(sqlc.arg(user_ids)::text[]);

-- name: GetChannelHistoryInfo :many
SELECT
    c."id" AS item_id,
    c.created_at,
    c.updated_at,
    uh.viewed_at,
    uh.interacted_at,
    c.owner_id AS user_id,
    c.channel_type
FROM comms_channels c
LEFT JOIN comms_activity uh ON uh.channel_id = c.id
    AND uh.user_id = $1
WHERE c.id = ANY(sqlc.arg(channel_ids)::uuid[])
ORDER BY c.updated_at DESC;
