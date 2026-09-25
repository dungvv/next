-- name: GetOrganizationSettings :one
SELECT
            o.name as name,
            orp.retention_days as "retention_days"
        FROM "Organization" o
        LEFT JOIN "OrganizationRetentionPolicy" orp ON o.id = orp.organization_id
        WHERE o.id = $1;

