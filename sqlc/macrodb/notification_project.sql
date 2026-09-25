-- name: GetProjectNotificationUsers :many
SELECT
            u."id" as id
            FROM "Project" p
            INNER JOIN "UserHistory" uh ON uh."itemId" = p."id" AND uh."itemType" = 'project'
            INNER JOIN "User" u ON u.id = uh."userId"
            WHERE p.id = $1;

