-- name: RemoveOrganizationRetentionPolicy :exec
DELETE FROM "OrganizationRetentionPolicy"
        WHERE "organization_id" = $1;

