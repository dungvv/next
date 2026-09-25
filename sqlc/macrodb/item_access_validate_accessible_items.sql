-- name: ValidateUserAccessibleItems :many
WITH user_source_ids AS (
            SELECT cp.channel_id::text as source_id FROM comms_channel_participants cp
                WHERE cp.user_id = $1 AND cp.left_at IS NULL
            UNION ALL
            SELECT t.team_id::text FROM team_user t
                WHERE t.user_id = $1
            UNION ALL
            SELECT $1
        ),
        -- Get all entities the user has access to via entity_access, filtered to requested items
        AllAccessGrants AS (
            -- Documents the user has access to
            SELECT ea.entity_id::text as item_id, ea.entity_type as item_type
            FROM entity_access ea
            LEFT JOIN "Document" d ON ea.entity_type = 'document' AND ea.entity_id::text = d.id
            WHERE ea.source_id = ANY(SELECT source_id FROM user_source_ids)
              AND ea.entity_type = 'document'
              AND ea.entity_id::"text" = ANY($2::text[])
              AND d."deletedAt" IS NULL

            UNION ALL

            -- Chats the user has access to
            SELECT ea.entity_id::text as item_id, ea.entity_type as item_type
            FROM entity_access ea
            LEFT JOIN "Chat" c ON ea.entity_type = 'chat' AND ea.entity_id::text = c.id
            WHERE ea.source_id = ANY(SELECT source_id FROM user_source_ids)
              AND ea.entity_type = 'chat'
              AND ea.entity_id::"text" = ANY($3::text[])
              AND c."deletedAt" IS NULL

            UNION ALL

            -- Projects the user has access to
            SELECT ea.entity_id::text as item_id, ea.entity_type as item_type
            FROM entity_access ea
            LEFT JOIN "Project" p ON ea.entity_type = 'project' AND ea.entity_id::text = p.id
            WHERE ea.source_id = ANY(SELECT source_id FROM user_source_ids)
              AND ea.entity_type = 'project'
              AND ea.entity_id::"text" = ANY($4::text[])
              AND p."deletedAt" IS NULL
        ),
        UserAccessibleItems AS (
            SELECT
                item_id,
                item_type
            FROM AllAccessGrants
            GROUP BY item_id, item_type
        )
        SELECT item_id as "item_id", item_type as "item_type" FROM UserAccessibleItems;

