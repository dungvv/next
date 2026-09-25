-- name: RevokeOrganizationInvitationForUser :exec
DELETE FROM "OrganizationInvitation" WHERE organization_id = $1 AND email = $2;


-- name: RevokeOrganizationInvitationForUser2 :exec
DELETE FROM "OrganizationEmailMatches" WHERE "organizationId" = $1 AND email = $2;

