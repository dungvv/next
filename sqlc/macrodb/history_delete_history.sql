-- name: DeleteUserHistory :exec
DELETE FROM "UserHistory" WHERE "userId" = $1 AND "itemId" = $2 AND "itemType" = $3;

