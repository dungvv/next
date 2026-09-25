-- name: DeleteChatMessage :one
DELETE FROM "ChatMessage"
          WHERE id = $1
          RETURNING id;

