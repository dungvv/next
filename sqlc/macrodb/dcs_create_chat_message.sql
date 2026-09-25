-- name: CreateChatMessage :one
INSERT INTO "ChatMessage" ("id", "chatId", "content", "role", "model", "createdAt", "updatedAt")
            VALUES ($1, $2, $3, $4, $5, $6, $7)
            RETURNING id;


-- name: CreateChatMessage2 :exec
INSERT INTO "ChatAttachment" ("entity_type", "entity_id", "chatId", "messageId")
            SELECT unnest($1::TEXT[]), unnest($2::UUID[]), unnest($3::TEXT[]), unnest($4::TEXT[]);

