-- name: AccessibleDocuments :many
WITH user_source_ids AS (
            SELECT cp.channel_id::text as source_id FROM comms_channel_participants cp
                WHERE cp.user_id = $1 AND cp.left_at IS NULL
            UNION ALL
            SELECT t.team_id::text FROM team_user t
                WHERE t.user_id = $1
            UNION ALL
            SELECT $1
        )
        SELECT DISTINCT ea.entity_id::text as "item_id"
        FROM entity_access ea
        JOIN user_source_ids us ON us.source_id = ea.source_id
        JOIN "Document" d ON d.id = ea.entity_id::text
        WHERE ea.entity_type = 'document'
            AND d."deletedAt" IS NULL
            AND (NOT $2::bool OR d.owner != $1);


-- name: AccessibleChats :many
WITH user_source_ids AS (
            SELECT cp.channel_id::text as source_id FROM comms_channel_participants cp
                WHERE cp.user_id = $1 AND cp.left_at IS NULL
            UNION ALL
            SELECT t.team_id::text FROM team_user t
                WHERE t.user_id = $1
            UNION ALL
            SELECT $1
        )
        SELECT DISTINCT ea.entity_id::text as "item_id"
        FROM entity_access ea
        JOIN user_source_ids us ON us.source_id = ea.source_id
        JOIN "Chat" c ON c.id = ea.entity_id::text
        WHERE ea.entity_type = 'chat'
            AND c."deletedAt" IS NULL
            AND (NOT $2::bool OR c."userId" != $1);


-- name: AccessibleProjects :many
WITH user_source_ids AS (
            SELECT cp.channel_id::text as source_id FROM comms_channel_participants cp
                WHERE cp.user_id = $1 AND cp.left_at IS NULL
            UNION ALL
            SELECT t.team_id::text FROM team_user t
                WHERE t.user_id = $1
            UNION ALL
            SELECT $1
        )
        SELECT DISTINCT ea.entity_id::text as "item_id"
        FROM entity_access ea
        JOIN user_source_ids us ON us.source_id = ea.source_id
        JOIN "Project" p ON p.id = ea.entity_id::text
        WHERE ea.entity_type = 'project'
            AND p."deletedAt" IS NULL
            AND (NOT $2::bool OR p."userId" != $1);

