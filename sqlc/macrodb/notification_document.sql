-- name: GetDocumentNotificationUsers :many
SELECT
            u."id" as id
            FROM "Document" d
            INNER JOIN "UserHistory" uh ON uh."itemId" = d."id" AND uh."itemType" = 'document'
            INNER JOIN "User" u ON u.id = uh."userId"
            WHERE d.id = $1 AND u.id != d."owner";

