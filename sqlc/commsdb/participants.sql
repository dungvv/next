-- Queries ported from crates/comms_db_client/src/participants/

-- name: GetChannelParticipants :many
-- Covers both get_participants and get_participants_tsx (identical SQL).
SELECT
    user_id,
    channel_id,
    joined_at,
    left_at,
    role
FROM comms_channel_participants
WHERE channel_id = $1
ORDER BY joined_at DESC;

-- name: GetChannelParticipantUserIDs :many
-- User ids of channel participants that need to be notified.
SELECT
    user_id
FROM comms_channel_participants
WHERE channel_id = $1;

-- name: GetChannelParticipantsForThread :many
-- Distinct active channel participants that either posted in the thread or
-- were user-mentioned in a thread message.
SELECT DISTINCT id FROM (
    SELECT m.sender_id AS id
    FROM comms_messages m
    JOIN comms_channel_participants cp
      ON cp.channel_id = m.channel_id AND cp.user_id = m.sender_id
    WHERE (m.id = $1 OR m.thread_id = $1)
      AND m.deleted_at IS NULL
      AND cp.left_at IS NULL
    UNION
    SELECT em.entity_id AS id
    FROM comms_entity_mentions em
    JOIN comms_messages m ON m.id::text = em.source_entity_id
    JOIN comms_channel_participants cp
      ON cp.channel_id = m.channel_id AND cp.user_id = em.entity_id
    WHERE (m.id = $1 OR m.thread_id = $1)
      AND m.deleted_at IS NULL
      AND em.source_entity_type = 'message'
      AND em.entity_type = 'user'
      AND cp.left_at IS NULL
) AS combined;

-- name: RemoveParticipant :exec
DELETE FROM comms_channel_participants
WHERE channel_id = $1 AND user_id = $2;
