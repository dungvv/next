-- name: DeleteUserProjects :many
SELECT id FROM "Project" WHERE "userId" = $1;


-- name: DeleteUserProjects2 :exec
DELETE FROM "SharePermission" sp
        USING "ProjectPermission" pp 
        WHERE pp."sharePermissionId" = sp.id
        AND pp."projectId" = ANY($1::text[]);


-- name: DeleteUserProjects3 :exec
DELETE FROM "Project" 
        WHERE "id" = ANY($1::text[]);

