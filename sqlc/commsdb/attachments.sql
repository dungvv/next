-- Queries ported from crates/comms_db_client/src/attachments/get_attachment_references.rs
-- The Rust function runs all three concurrently and merges/sorts in memory.

-- name: GetAttachmentReferences :many
-- Channel references where the entity was attached to a message in a channel
-- the given user is an active participant of.
SELECT
    a.channel_id,
    c.name AS channel_name,
    a.message_id,
    m.thread_id,
    m.sender_id,
    m.content AS message_content,
    m.created_at AS message_created_at,
    a.created_at AS attachment_created_at
FROM comms_attachments a
JOIN comms_messages m ON a.message_id = m.id
JOIN comms_channels c ON a.channel_id = c.id
JOIN comms_channel_participants cp ON cp.channel_id = c.id
WHERE a.entity_type = $1
  AND a.entity_id  = $2
  AND cp.user_id   = $3
  AND cp.left_at  IS NULL
  AND m.deleted_at IS NULL
ORDER BY a.created_at DESC;

-- name: GetMentionReferences :many
-- Channel references where the entity was mentioned inside a message.
SELECT
    m.channel_id,
    c.name AS channel_name,
    m.id AS message_id,
    m.thread_id,
    m.sender_id,
    m.content AS message_content,
    m.created_at AS message_created_at,
    em.created_at AS attachment_created_at
FROM comms_entity_mentions em
JOIN comms_messages m ON (em.source_entity_id = m.id::text AND em.source_entity_type = 'message')
JOIN comms_channels c ON m.channel_id = c.id
JOIN comms_channel_participants cp ON cp.channel_id = c.id
WHERE em.entity_type = $1
  AND em.entity_id  = $2
  AND cp.user_id   = $3
  AND cp.left_at  IS NULL
  AND m.deleted_at IS NULL
ORDER BY em.created_at DESC;

-- name: GetGenericEntityMentionReferences :many
-- Mentions of the entity from non-message sources (no access check upstream).
SELECT
    em.source_entity_type,
    em.source_entity_id,
    em.entity_type,
    em.entity_id,
    em.user_id,
    em.created_at
FROM comms_entity_mentions em
WHERE em.entity_type = $1
  AND em.entity_id  = $2
  AND em.source_entity_type != 'message'
ORDER BY em.created_at DESC;
