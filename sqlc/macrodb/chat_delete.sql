-- name: SoftDeleteChat :exec
DELETE FROM "UserHistory" WHERE "itemId" = $1 AND "itemType" = $2;


-- name: SoftDeleteChat2 :exec
UPDATE "Chat" SET "deletedAt" = NOW() WHERE id = $1;


-- name: DeleteChatBulkTsx :exec
DELETE FROM "Pin" WHERE "pinnedItemId" = ANY($1::text[]) AND "pinnedItemType" = $2;


-- name: DeleteChatBulkTsx2 :exec
DELETE FROM "UserHistory" WHERE "itemId" = ANY($1::text[]) AND "itemType" = $2;


-- name: DeleteChatBulkTsx3 :exec
DELETE FROM "SharePermission"
            WHERE id IN (
                SELECT "sharePermissionId"
                FROM "ChatPermission"
                WHERE "chatId" = ANY($1::text[])
            );


-- name: DeleteChatBulkTsx4 :exec
DELETE FROM "Chat"
        WHERE "id" = ANY($1::text[]);


-- name: DeleteChat :one
SELECT "sharePermissionId" as share_permission_id
            FROM "ChatPermission"
            WHERE "chatId"=$1;


-- name: DeleteChat2 :exec
DELETE FROM "SharePermission" WHERE id = $1;


-- name: DeleteChat3 :exec
DELETE FROM "Chat"
        WHERE id = $1;

