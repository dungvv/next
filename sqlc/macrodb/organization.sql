-- name: DeleteOrganizationInvitation :exec
DELETE FROM "OrganizationInvitation"
        WHERE email = $1;

