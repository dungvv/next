-- name: CopyMessages :exec
INSERT INTO "ChatMessage" ("chatId", "createdAt", "updatedAt", "content", "role", "model")
        SELECT $1, "createdAt", "updatedAt", "content", "role", "model"
        FROM "ChatMessage"
        WHERE "ChatMessage"."chatId"=$2;

