-- name: GetUsersThatDoNotExist :many
SELECT "id"
        FROM "User"
        WHERE "id" = ANY($1::text[]);

