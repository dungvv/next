-- name: GetSharePermissionId :one
SELECT
                        sp.id as id
                    FROM
                        "DocumentPermission" dp
                    JOIN "SharePermission" sp ON dp."sharePermissionId" = sp.id
                    WHERE
                        dp."documentId" = $1;


-- name: GetSharePermissionId2 :one
SELECT
                        sp.id as id
                    FROM
                        "ChatPermission" cp
                    JOIN "SharePermission" sp ON cp."sharePermissionId" = sp.id
                    WHERE
                        cp."chatId" = $1;


-- name: GetSharePermissionId3 :one
SELECT
                        sp.id as id
                    FROM
                        "EmailThreadPermission" tp
                    JOIN "SharePermission" sp ON tp."sharePermissionId" = sp.id
                    WHERE
                        tp."threadId" = $1;


-- name: GetSharePermissionId4 :one
SELECT
                        sp.id as id
                    FROM
                        "ProjectPermission" pp
                    JOIN "SharePermission" sp ON pp."sharePermissionId" = sp.id
                    WHERE
                        pp."projectId" = $1;


-- name: GetSharePermissionId5 :one
SELECT share_permission_id as "share_permission_id"
            FROM (
            SELECT share_permission_id FROM calls WHERE calls.id = $1
            UNION ALL
            SELECT share_permission_id FROM call_records WHERE call_records.id = $1
        ) t
        LIMIT 1;


-- name: GetDocumentSharePermission :one
SELECT
                sp.id as id,
                sp."linkShare" as "link_share",
                sp."linkShareAccessLevel" as "link_share_access_level",
                sp.team_share_access_level as "team_share_access_level",
                d."owner" as owner,
                COALESCE(
                    json_agg(json_build_object(
                        'channel_id', csp."channel_id",
                        'access_level', csp."access_level"
                    )) FILTER (WHERE csp."channel_id" IS NOT NULL),
                    '[]'::jsonb
                )::jsonb as "channel_share_permissions"
            FROM
                "DocumentPermission" dp
            JOIN "SharePermission" sp ON dp."sharePermissionId" = sp.id
            JOIN "Document" d ON dp."documentId" = d.id
            LEFT JOIN "ChannelSharePermission" csp ON csp."share_permission_id" = sp.id
            WHERE
                dp."documentId" = $1
            GROUP BY
                sp.id, d."owner";


-- name: GetChatSharePermission :one
SELECT
                sp.id as id,
                sp."linkShare" as "link_share",
                sp."linkShareAccessLevel" as "link_share_access_level",
                sp.team_share_access_level as "team_share_access_level",
                c."userId" as owner,
                COALESCE(
                    json_agg(json_build_object(
                        'channel_id', csp."channel_id",
                        'access_level', csp."access_level"
                    )) FILTER (WHERE csp."channel_id" IS NOT NULL),
                    '[]'::jsonb
                )::jsonb as "channel_share_permissions"
            FROM
                "ChatPermission" cp
            JOIN "SharePermission" sp ON cp."sharePermissionId" = sp.id
            JOIN "Chat" c ON cp."chatId" = c.id
            LEFT JOIN "ChannelSharePermission" csp ON csp."share_permission_id" = sp.id
            WHERE
                cp."chatId" = $1
            GROUP BY
                sp.id, c."userId";


-- name: GetMacroSharePermission :one
SELECT
                sp.id as id,
                sp."linkShare" as "link_share",
                sp."linkShareAccessLevel" as "link_share_access_level",
                sp.team_share_access_level as "team_share_access_level",
                m."user_id" as owner,
                COALESCE(
                    json_agg(json_build_object(
                        'channel_id', csp."channel_id",
                        'access_level', csp."access_level"
                    )) FILTER (WHERE csp."channel_id" IS NOT NULL),
                    '[]'::jsonb
                )::jsonb as "channel_share_permissions"
            FROM
                "MacroPromptPermission" mpp
            JOIN "SharePermission" sp ON mpp."share_permission_id" = sp.id
            JOIN "MacroPrompt" m ON mpp."macro_prompt_id" = m.id
            LEFT JOIN "ChannelSharePermission" csp ON csp."share_permission_id" = sp.id
            WHERE
                mpp."macro_prompt_id" = $1
            GROUP BY
                sp.id, m."user_id";


-- name: GetEmailThreadPermission :one
SELECT 
            "threadId" as thread_id,
            "sharePermissionId" as share_permission_id,
            "userId" as user_id,
            "projectId" as project_id
        FROM "EmailThreadPermission"
        WHERE "threadId" = $1;


-- name: GetMacroIdFromThreadId :one
SELECT l.macro_id
        FROM email_threads t
        JOIN email_links l ON t.link_id = l.id
        WHERE t.id = $1;


-- name: CheckChannelsForUser :many
SELECT c.id
        FROM comms_channels c
        INNER JOIN comms_channel_participants cp ON cp.channel_id = c.id 
        WHERE cp.user_id = $1 AND cp.left_at IS NULL
        AND c."id" = ANY($2::uuid[]);


-- name: GetItemsBySharePermissionIds :many
SELECT 'document' as "item_type", "documentId" as "item_id", "sharePermissionId" as "share_permission_id"
        FROM "DocumentPermission"
        WHERE "sharePermissionId" = ANY($1::text[])
        UNION ALL
        SELECT 'chat' as "item_type", "chatId" as "item_id", "sharePermissionId" as "share_permission_id"
        FROM "ChatPermission"
        WHERE "sharePermissionId" = ANY($1::text[])
        UNION ALL
        SELECT 'project' as "item_type", "projectId" as "item_id", "sharePermissionId" as "share_permission_id"
        FROM "ProjectPermission"
        WHERE "sharePermissionId" = ANY($1::text[])
        UNION ALL
        SELECT 'thread' as "item_type", "threadId" as "item_id", "sharePermissionId" as "share_permission_id"
        FROM "EmailThreadPermission"
        WHERE "sharePermissionId" = ANY($1::text[]);

