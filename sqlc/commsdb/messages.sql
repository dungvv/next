-- Queries ported from crates/comms_db_client/src/messages/

-- name: CreateMessage :one
-- Covers both create_message (caller generates uuid v7) and seed_message
-- (caller supplies the id) -- the SQL is identical.
INSERT INTO comms_messages (id, channel_id, sender_id, content, thread_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING
    id,
    channel_id,
    sender_id,
    content,
    created_at,
    updated_at,
    thread_id,
    edited_at,
    deleted_at;

-- name: CreateMessageMentions :many
-- Inserts message entity mentions (deduped) and returns the user_ids of
-- mentioned users that are active participants of the message's channel.
WITH message_channel AS (
    SELECT comms_messages.channel_id FROM comms_messages WHERE comms_messages.id = sqlc.arg(message_id)
),
mentions_to_insert AS (
    -- sqlc only supports single-array UNNEST; pair by ordinality.
    SELECT et.entity_type, ei.entity_id
    FROM UNNEST(sqlc.arg(entity_types)::text[]) WITH ORDINALITY AS et(entity_type, ord)
    JOIN UNNEST(sqlc.arg(entity_ids)::text[]) WITH ORDINALITY AS ei(entity_id, ord) ON ei.ord = et.ord
),
inserted_mentions AS (
    INSERT INTO comms_entity_mentions (id, source_entity_type, source_entity_id, entity_type, entity_id, user_id)
    SELECT gen_random_uuid(), 'message', sqlc.arg(message_id)::text, m.entity_type, m.entity_id, NULL
    FROM mentions_to_insert m
    WHERE NOT EXISTS (
        SELECT 1 FROM comms_entity_mentions em
        WHERE em.source_entity_type = 'message'
          AND em.source_entity_id = sqlc.arg(message_id)::text
          AND em.entity_type = m.entity_type
          AND em.entity_id = m.entity_id
    )
)
SELECT DISTINCT cp.user_id
FROM mentions_to_insert m
CROSS JOIN message_channel mc
JOIN comms_channel_participants cp ON m.entity_id = cp.user_id
WHERE m.entity_type = 'user'
AND cp.channel_id = mc.channel_id
AND cp.left_at IS NULL;

-- name: GetMessageMentionEntities :many
-- entity_type/entity_id pairs mentioned by a message (SimpleMention list).
SELECT
    entity_type,
    entity_id
FROM comms_entity_mentions
WHERE source_entity_id = sqlc.arg(message_id) AND source_entity_type = 'message';

-- name: GetChannelMessage :one
-- Channel + message info used to populate channel messages for search.
SELECT
    c.id AS channel_id,
    c.name AS name,
    c.channel_type,
    c.org_id,
    m.id AS message_id,
    m.thread_id,
    m.sender_id,
    m.content,
    m.created_at,
    m.updated_at,
    m.deleted_at::timestamptz AS deleted_at
FROM comms_messages m
JOIN comms_channels c ON c."id" = m."channel_id"
WHERE m.id = $1 AND c.id = $2;

-- name: GetMessageDeletionStates :many
SELECT
    id,
    deleted_at::timestamptz AS deleted_at
FROM comms_messages
WHERE id = ANY(sqlc.arg(message_ids)::uuid[]);

-- name: GetMessages :many
-- Latest messages for a channel: most recent first, capped, then re-ordered
-- ascending. `since`/`message_limit` may be NULL (no filter / no limit).
SELECT
    id,
    channel_id,
    sender_id,
    content,
    created_at,
    updated_at,
    thread_id,
    edited_at,
    deleted_at
FROM (
    SELECT *
    FROM comms_messages
    WHERE channel_id = $1
    AND (sqlc.narg(since)::timestamptz IS NULL OR created_at >= sqlc.narg(since))
    ORDER BY created_at DESC
    LIMIT sqlc.narg(message_limit)::bigint
) AS latest_messages
ORDER BY created_at ASC;

-- name: GetChannelMessages :many
-- Paginated (channel_id, message_id) pairs for search backfill.
-- `only_deleted` tri-state: NULL = all, true = only deleted, false = only active.
SELECT
    channel_id,
    id
FROM comms_messages
WHERE channel_id IS NOT NULL
AND (
    sqlc.narg(only_deleted)::bool IS NULL
    OR (sqlc.narg(only_deleted) AND deleted_at IS NOT NULL)
    OR (NOT sqlc.narg(only_deleted) AND deleted_at IS NULL)
)
ORDER BY created_at ASC
LIMIT sqlc.arg(row_limit)
OFFSET sqlc.arg(row_offset);
