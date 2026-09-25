-- name: DeleteUserChats :many
SELECT id FROM "Chat" WHERE "userId" = $1;


-- name: DeleteUserChats2 :exec
DELETE FROM "Pin" 
        WHERE "pinnedItemId" = ANY($1::text[]) AND "pinnedItemType" = $2;


-- name: DeleteUserChats3 :exec
DELETE FROM "UserHistory" 
        WHERE "itemId" = ANY($1::text[]) AND "itemType" = $2;


-- name: DeleteUserChats4 :exec
DELETE FROM "SharePermission" sp
        USING "ChatPermission" cp 
        WHERE cp."sharePermissionId" = sp.id
        AND cp."chatId" = ANY($1::text[]);


-- name: DeleteUserChats5 :exec
DELETE FROM "Chat" 
        WHERE "id" = ANY($1::text[]);

