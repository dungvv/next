-- name: RemoveOrganizationDefaultSharePermission :exec
DELETE FROM "OrganizationDefaultSharePermission"
        WHERE "organization_id" = $1;

