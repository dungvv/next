-- name: GetStripeCustomerIdByUserId :one
SELECT "stripeCustomerId" as "stripe_customer_id"
        FROM "User"
        WHERE "id" = $1;


-- name: GetUserMacroIdByEmail :one
SELECT "macro_user_id" as "macro_user_id"
        FROM "User"
        WHERE "email" = $1;


-- name: GetUserIdByEmail :one
SELECT "id"
        FROM "User"
        WHERE "email" = $1;


-- name: GetMacroUserIdByEmail :one
SELECT "macro_user_id"
        FROM "User"
        WHERE "email" = $1;


-- name: GetUserIdAndStripeCustomerIdByEmail :one
SELECT "id", "stripeCustomerId" as "stripe_customer_id"
        FROM "User"
        WHERE "email" = $1;


-- name: GetUserProfileByFusionauthUserIdAndEmail :one
SELECT id, "organizationId" as "organization_id"
        FROM "User"
        WHERE "macro_user_id" = $1
        AND LOWER(email) = LOWER($2);


-- name: GetUserProfilesByFusionauthUserId :many
SELECT id
        FROM "User"
        WHERE "macro_user_id" = $1;


-- name: GetUserInfoByEmail :one
SELECT 
            id,
            email,
            "organizationId" as "organization_id",
            macro_user_id as "macro_user_id"
        FROM "User"
        WHERE "email" = $1;


-- name: GetUserProfile :one
SELECT 
            id,
            email,
            "organizationId" as "organization_id"
        FROM "User"
        WHERE "id" = $1;


-- name: GetUserMacroUserIdAndIdByEmail :one
SELECT "macro_user_id" as "macro_user_id", "id"
        FROM "User"
        WHERE "email" = $1;


-- name: GetUserEmails :one
SELECT COUNT(*) as "count"
            FROM "User" u;


-- name: GetUserEmails2 :many
SELECT
            u.email
        FROM
            "User" u
        LIMIT $1 OFFSET $2;

