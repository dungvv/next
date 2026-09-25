-- name: GetInvitedUsersByOrganization :many
SELECT
            oi.email
        FROM "OrganizationInvitation" oi
        WHERE oi.organization_id = $1;

