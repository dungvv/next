-- name: CreateMacroUser :exec
INSERT INTO macro_user (id, username, stripe_customer_id, email)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT ("id") DO NOTHING;


-- name: GetMacroUser :one
SELECT id, username, stripe_customer_id
        FROM macro_user
        WHERE id = $1;


-- name: DeleteMacroUser :exec
DELETE FROM macro_user
        WHERE id = $1;


-- name: CheckEmailExists :one
SELECT id
        FROM "User"
        WHERE email = $1;


-- name: CheckUsernameExists :one
SELECT id
        FROM macro_user
        WHERE username = $1;

