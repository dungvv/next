-- name: GetUserByEmail :one
SELECT
            u.id,
            u."organizationId" as organization_id
        FROM
            "User" u
        WHERE
            u.email = $1;


-- name: GetUserEmail :one
SELECT
            u.email
        FROM
            "User" u
        WHERE
            u.id = $1;


-- name: GetUsersIdsFromEmails :many
SELECT
            u.id
        FROM
            "User" u
        WHERE
            u."email" = ANY($1::text[]);

