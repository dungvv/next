-- name: GetChatNotificationUsers :many
SELECT
            u."id" as id
            FROM "Chat" c
            INNER JOIN "UserHistory" uh ON uh."itemId" = c."id" AND uh."itemType" = 'chat'
            INNER JOIN "User" u ON u.id = uh."userId"
            WHERE c.id = $1 AND u.id != c."userId";

