-- name: GetHighestAccessLevelForChats :many
WITH user_source_ids AS (
            SELECT cp.channel_id::text as source_id FROM comms_channel_participants cp
                WHERE cp.user_id = $2 AND cp.left_at IS NULL
            UNION ALL
            SELECT t.team_id::text FROM team_user t
                WHERE t.user_id = $2
            UNION ALL
            SELECT $2
        )
        SELECT
            chat_id,
            access_level
        FROM (
            -- Source 1: entity_access for chats
            SELECT
                ea.entity_id::text as chat_id,
                ea.access_level::text as access_level
            FROM entity_access ea
            WHERE ea.source_id = ANY(SELECT source_id FROM user_source_ids)
                AND ea.entity_id = ANY(SELECT id::uuid FROM "Chat" WHERE "id" = ANY($1::text[]) AND "deletedAt" IS NULL)
                AND ea.entity_type = 'chat'
            UNION ALL
            -- Source 2: Direct chat link permissions
            SELECT
                c.id as chat_id,
                sp."linkShareAccessLevel"::text as access_level
            FROM "Chat" c
            JOIN "ChatPermission" cp ON cp."chatId" = c.id
            JOIN "SharePermission" sp ON sp.id = cp."sharePermissionId"
                AND sp."linkShareAccessLevel" IS NOT NULL
                AND (
                    sp."linkShare" = 'PUBLIC'
                    OR (
                        sp."linkShare" = 'TEAM'
                        AND EXISTS (
                            SELECT 1
                            FROM team_user owner_team
                            WHERE owner_team.user_id = c."userId"
                                AND owner_team.team_id::text = ANY(
                                    SELECT source_id FROM user_source_ids
                                )
                        )
                    )
                )
            WHERE c."id" = ANY($1::text[]) AND c."deletedAt" IS NULL
        ) as all_levels;

