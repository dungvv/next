-- name: GetUserByEmail2 :one
SELECT "id" as user_id, "organizationId" as "organization_id" FROM "User" WHERE "email" = $1;

