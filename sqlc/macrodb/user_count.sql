-- name: CountUserItems :one
SELECT COUNT(*) as "count"
            FROM "Document" d
            WHERE owner = $1 AND d."deletedAt" IS NULL;


-- name: CountUserItems2 :one
SELECT COUNT(*) as "count"
            FROM "Chat" c
            WHERE c."userId" = $1 and c."deletedAt" IS NULL;

