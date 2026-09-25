-- name: BulkGetAllSubProjectIds :many
WITH RECURSIVE project_hierarchy AS (
            SELECT
                p.id
            FROM "Project" p
            WHERE p."id" = ANY($1::text[]) AND p."deletedAt" IS NULL
            UNION ALL
            SELECT
                sub_p.id
            FROM "Project" sub_p
            INNER JOIN project_hierarchy ph ON sub_p."parentId" = ph.id
            WHERE sub_p."deletedAt" IS NULL
        )
        SELECT
            ph.id as "id"
        FROM project_hierarchy ph;

