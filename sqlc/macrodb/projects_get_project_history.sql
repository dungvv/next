-- name: GetProjectHistoryInfo :many
SELECT
            p."id" as "item_id",
            p."createdAt" as "created_at",
            p."updatedAt" as "updated_at",
            p."deletedAt" as "deleted_at",
            p."parentId" as "parent_project_id",
            uh."updatedAt" as "viewed_at",
            p."userId" as "user_id",
            p."name"
        FROM
            "Project" p
        LEFT JOIN
            "UserHistory" uh ON uh."itemId" = p."id"
                AND uh."userId" = $1
                AND uh."itemType" = 'project'
        WHERE
            p."id" = ANY($2::text[])
        ORDER BY
            p."updatedAt" DESC;

