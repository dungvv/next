-- name: InviteUser :exec
INSERT INTO "OrganizationInvitation" ("organization_id", "email")
        VALUES ($1, $2);


-- name: InviteUser2 :exec
INSERT INTO "OrganizationEmailMatches" ("organizationId", "email")
            VALUES ($1, $2)
            ON CONFLICT ("email") DO NOTHING;

