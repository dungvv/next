-- name: DeleteProjectsBulkTsx :exec
DELETE FROM "SharePermission"
            WHERE id IN (
                SELECT "sharePermissionId"
                FROM "ProjectPermission"
                WHERE "projectId" = ANY($1::text[])
            );


-- name: DeleteProjectsBulkTsx2 :exec
DELETE FROM "Project"
        WHERE "id" = ANY($1::text[]);

