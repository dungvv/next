-- name: GetUserPermissions2 :many
SELECT
        rop."permissionId" as permission_id
        FROM "User" u
        JOIN "RolesOnUsers" ru ON ru."userId" = u.id
        JOIN "RolesOnPermissions" rop ON rop."roleId" = ru."roleId"
        WHERE u.id = $1;


-- name: GetAllPermissions :many
SELECT 
            id,
            description 
        FROM "Permission";

