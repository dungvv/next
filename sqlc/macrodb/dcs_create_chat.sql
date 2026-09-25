-- name: CreateChatV2 :one
INSERT INTO "Chat" ("userId", name, model, "projectId", "isPersistent")
                VALUES ($1, $2, $3, $4, $5)
                RETURNING id, "createdAt"::timestamptz as created_at, "updatedAt"::timestamptz as updated_at;

