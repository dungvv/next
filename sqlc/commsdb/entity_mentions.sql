-- Queries ported from crates/comms_db_client/src/entity_mentions/

-- name: CreateEntityMentions :execrows
-- Bulk insert from parallel arrays. The Rust version built per-row copies of
-- source_entity_type/source_entity_id/user_id and passed a 6-array UNNEST;
-- sqlc only supports single-array UNNEST so the uniform values are scalar
-- params joined by ordinality (identical semantics).
INSERT INTO comms_entity_mentions (id, source_entity_type, source_entity_id, entity_type, entity_id, user_id)
SELECT a.id, sqlc.arg(source_entity_type), sqlc.arg(source_entity_id), b.entity_type, c.entity_id, sqlc.narg(user_id)
FROM UNNEST(sqlc.arg(ids)::uuid[]) WITH ORDINALITY AS a(id, ord)
JOIN UNNEST(sqlc.arg(entity_types)::varchar[]) WITH ORDINALITY AS b(entity_type, ord) ON b.ord = a.ord
JOIN UNNEST(sqlc.arg(entity_ids)::varchar[]) WITH ORDINALITY AS c(entity_id, ord) ON c.ord = a.ord;

-- name: DeleteEntityMentionsBySource :execrows
DELETE FROM comms_entity_mentions
WHERE source_entity_id = ANY(sqlc.arg(source_entity_ids)::varchar[]);
