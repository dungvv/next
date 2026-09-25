-- name: GetUserOrganization2 :one
SELECT "organizationId" as organization_id FROM "User" WHERE id = $1;

