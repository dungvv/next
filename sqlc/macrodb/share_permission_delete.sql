-- name: DeleteSharePermission :exec
DELETE FROM "SharePermission"
            WHERE "id" = $1;


-- name: DeleteProjectSharePermission :one
SELECT pp."sharePermissionId" as id
        FROM "ProjectPermission" pp
        WHERE
            pp."projectId" = $1;


-- name: DeleteDocumentSharePermission :exec
DELETE FROM "DocumentPermission"
            WHERE "sharePermissionId" = $1;


-- name: DeleteChatSharePermission :exec
DELETE FROM "ChatPermission"
            WHERE "sharePermissionId" = $1;

