-- name: GetUserPermissions :many
SELECT
          rp."roleId" AS role_id,
          rp."permissionId" AS permission_id
        FROM
          "User" u
        INNER JOIN
          "RolesOnUsers" ru ON u.id = ru."userId"
        INNER JOIN
          "RolesOnPermissions" rp ON ru."roleId" = rp."roleId"
        WHERE
          u.id = $1;

