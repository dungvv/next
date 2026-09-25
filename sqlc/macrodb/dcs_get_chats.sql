-- name: GetChats :many
SELECT
            c.id,
            c.name,
            c.model,
            c."tokenCount" as "token_count",
            c."userId" as "user_id",
            c."createdAt"::timestamptz as "created_at",
            c."updatedAt"::timestamptz as "updated_at",
            c."deletedAt"::timestamptz as "deleted_at",
            c."projectId" as "project_id",
            c."isPersistent" as "is_persistent"
        FROM "Chat" c
        WHERE c."userId" = $1 AND c."deletedAt" IS NULL;

