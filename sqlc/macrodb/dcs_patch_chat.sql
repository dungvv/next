-- name: PatchChatTransaction :exec
UPDATE "Chat" SET "name" = $1
            WHERE id = $2;


-- name: PatchChatTransaction2 :exec
UPDATE "Chat" SET "model" = $1
            WHERE id = $2;


-- name: PatchChatTransaction3 :exec
UPDATE "Chat" SET "projectId" = NULL
            WHERE id = $1;


-- name: PatchChatTransaction4 :exec
UPDATE "Chat" SET "projectId" = $1
            WHERE id = $2;


-- name: PatchChatTransaction5 :exec
UPDATE "Chat" SET "tokenCount" = $1
            WHERE id = $2;

