-- name: CreateOrganizationRetentionPolicy :exec
INSERT INTO "OrganizationRetentionPolicy" (organization_id, retention_days)
    VALUES ($1, $2);

