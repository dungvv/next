-- name: GetOrganizationEmailMatches :many
SELECT oem.email
        FROM "OrganizationEmailMatches" oem
        JOIN "Organization" o ON o."id" = oem."organizationId"
        WHERE oem."organizationId" = $1;

