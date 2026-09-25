-- name: AddUserRole :exec
INSERT INTO "RolesOnUsers" ("userId", "roleId")
        VALUES ($1, $2);

