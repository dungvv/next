-- name: PatchOrganizationRetentionPolicy :one
SELECT id
            FROM "OrganizationRetentionPolicy"
            WHERE organization_id = $1;


-- name: PatchOrganizationRetentionPolicy2 :exec
UPDATE "OrganizationRetentionPolicy" 
        SET "retention_days" = $2
        WHERE "organization_id" = $1;

