-- name: GetBasicProject :one
SELECT
                p.id,
                p."userId" as user_id,
                p."name" as name,
                p."parentId" as parent_id,
                p."deletedAt"::timestamptz as "deleted_at"
            FROM "Project" p
            WHERE id = $1;

