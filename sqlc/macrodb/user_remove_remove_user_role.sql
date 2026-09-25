-- name: RemoveUserRole :exec
DELETE FROM "RolesOnUsers" WHERE "userId" = $1 AND "roleId" = $2;

