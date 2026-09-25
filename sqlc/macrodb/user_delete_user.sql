-- name: DeleteUser :one
DELETE FROM "User" WHERE id = $1 AND macro_user_id = $2 RETURNING id, email, "organizationId" as organization_id;

