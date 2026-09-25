-- name: GetProjectsForSearchBackfill :many
SELECT
                p.id as project_id,
                p."updatedAt"::timestamptz as updated_at
            FROM "Project" p
            WHERE p."deletedAt" IS NULL
                AND (
                    $2::timestamptz IS NULL
                    OR (p."updatedAt", p.id) > ($2, $3)
                )
                AND ($4::timestamptz IS NULL OR p."updatedAt" >= $4)
                AND ($5::timestamptz IS NULL OR p."updatedAt" <= $5)
            ORDER BY p."updatedAt" ASC, p.id ASC
            LIMIT $1;


-- name: GetSubProjectIds :many
SELECT
                p.id
            FROM
                "Project" p
            WHERE
                p."parentId" = ANY($1::text[]);


-- name: GetProjectsToDelete :many
SELECT p.id as project_id, p."userId" as user_id
            FROM "Project" p
            WHERE p."deletedAt" IS NOT NULL AND p."deletedAt" <= $1;


-- name: GetAllProjectIdsWithUsersPaginated :many
SELECT
            id,
            "userId" as user_id
        FROM
            "Project"
        WHERE
            "deletedAt" IS NULL
        ORDER BY
            "createdAt" DESC
        LIMIT $1 OFFSET $2;

