-- name: AddAttachment :one
INSERT INTO "ChatAttachment" ("entity_type", "entity_id", "chatId")
            VALUES ($1, $2, $3)
            RETURNING id;


-- name: AppendAttachmentToChat :exec
UPDATE "Chat" SET "updatedAt" = NOW()
        WHERE id = $1;

