-- name: GetBasicChat :one
SELECT
            c.id as "id",
            c.name as "name",
            c."projectId" as "project_id",
            c."userId" as "user_id",
            c."deletedAt"::timestamptz as "deleted_at"
        FROM
            "Chat" c
        WHERE
            c.id = $1;


-- name: GetChatsToDelete :many
SELECT c.id
            FROM "Chat" c
            WHERE c."deletedAt" IS NOT NULL AND c."deletedAt" <= $1;


-- name: GetChatIdsForMessages :many
SELECT DISTINCT m."chatId" as chat_id
        FROM "ChatMessage" m
        WHERE m."id" = ANY($1::text[]);

