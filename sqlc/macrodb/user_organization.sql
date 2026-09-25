-- name: MatchUserToOrganization :one
SELECT "organizationId" as organization_id
        FROM "OrganizationEmailMatches"
        WHERE "email" = ANY($1::text[]);


-- name: GetOrganizationRolesForUser :many
SELECT "roleId" as id
        FROM "RolesOnOrganizations"
        WHERE "organizationId" = $1;


-- name: GetOrganizationRolesForUser2 :one
SELECT "organizationId" as id
        FROM "OrganizationIT"
        WHERE "email" = $1;


-- name: GetOrganizationRolesForUser3 :one
SELECT "organizationId" as id
        FROM "OrganizationBilling"
        WHERE "email" = $1;

