-- name: GetLatestSingleAttachmentChat :one
WITH SingleAttachmentChats AS (
                SELECT
                    "chatId",
                    COUNT(*) as attachment_count
                FROM "ChatAttachment"
                GROUP BY "chatId"
                HAVING COUNT(*) = 1
            )
            SELECT
                c.id,
                c.name,
                c."userId" as "user_id",
                c."createdAt"::timestamptz as "created_at",
                c."updatedAt"::timestamptz as "updated_at",
                c."deletedAt"::timestamptz as "deleted_at",
                c.model,
                c."tokenCount" as "token_count",
                c."projectId" as "project_id",
                c."isPersistent" as "is_persistent"
            FROM "Chat" c
            INNER JOIN "ChatAttachment" ca ON c.id = ca."chatId"
            INNER JOIN SingleAttachmentChats sac ON c.id = sac."chatId"
            WHERE
                ca."entity_id"::TEXT = $1 AND c."userId" = $2
                AND
                c."deletedAt" IS NULL

            ORDER BY c."updatedAt" DESC
            LIMIT 1;


-- name: GetMultiAttachmentChat :many
WITH MultiAttachmentChats AS (
                SELECT
                    "chatId",
                    COUNT(*) as attachment_count
                FROM "ChatAttachment"
                GROUP BY "chatId"
            )
            SELECT
                c.id,
                c.name,
                c."userId" as "user_id",
                c."createdAt"::timestamptz as "created_at",
                c."updatedAt"::timestamptz as "updated_at",
                c."deletedAt"::timestamptz as "deleted_at",
                c.model,
                c."tokenCount" as "token_count",
                c."projectId" as "project_id",
                c."isPersistent" as "is_persistent"
            FROM "Chat" c
            INNER JOIN "ChatAttachment" ca ON c.id = ca."chatId"
            INNER JOIN MultiAttachmentChats mac ON c.id = mac."chatId"
            WHERE
                ca."entity_id"::TEXT = $1 AND c."userId" = $2
                    AND
                c."deletedAt" IS NULL
            ORDER BY c."updatedAt" DESC;

