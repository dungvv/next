-- name: UpdateChatTokenCount :exec
UPDATE "Chat" SET "tokenCount" = $1
        WHERE id = $2;

