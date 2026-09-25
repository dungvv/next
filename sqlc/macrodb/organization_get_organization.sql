-- name: GetAllowListOnly :one
SELECT "allowListOnly" as allow_list_only
        FROM "Organization"
        WHERE id = $1;


-- name: GetOrganizationName :one
SELECT
            name
        FROM
            "Organization"
        WHERE id = $1;

