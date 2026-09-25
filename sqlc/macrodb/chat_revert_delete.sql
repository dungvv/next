-- name: RevertDeleteChat :one
UPDATE "Chat"
        SET "deletedAt" = NULL
        WHERE id = $1
        RETURNING "userId" as owner;


-- name: RevertDeleteChat2 :exec
INSERT INTO "UserHistory" ("userId", "itemId", "itemType", "createdAt", "updatedAt")
        VALUES ($1, $2, $3, NOW(), NOW())
        ON CONFLICT ("userId", "itemId", "itemType") DO UPDATE
        SET "updatedAt" = NOW();


-- name: RevertDeleteChat3 :one
SELECT "deletedAt" as deleted_at FROM "Project" WHERE "id" = $1;


-- name: RevertDeleteChat4 :exec
UPDATE "Chat" SET "projectId" = NULL WHERE "id" = $1;

