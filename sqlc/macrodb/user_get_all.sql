-- name: GetExistingUsers :many
SELECT
            u.id
        FROM "User" u
        WHERE u."id" = ANY($1::text[]);


-- name: GetAllUserIdsStripeCustomerIdWithNullMacroUserId :many
SELECT
                u.id,
                u."stripeCustomerId" as "stripe_customer_id"
            FROM "User" u
            WHERE u."macro_user_id" IS NULL
                AND u.id > $1
            ORDER BY u.id ASC
            LIMIT $2;


-- name: GetAllUserIdsStripeCustomerIdWithNullMacroUserId2 :many
SELECT
                u.id,
                u."stripeCustomerId" as "stripe_customer_id"
            FROM "User" u
            WHERE u."macro_user_id" IS NULL
            ORDER BY u.id ASC
            LIMIT $1;


-- name: GetUserIdsByOrganization :one
SELECT COUNT(*) as "count" FROM "User" WHERE "organizationId" = $1;


-- name: GetUserIdsByOrganization2 :many
SELECT
            u.id as user_id
        FROM "User" u
        WHERE u."organizationId" = $1
        LIMIT $2
        OFFSET $3;


-- name: GetUsersByOrganization :many
SELECT
            u.id as user_id,
            u.email as user_email,
            array_agg(DISTINCT rop."permissionId")::text[] AS permissions
        FROM "User" u
        LEFT JOIN "RolesOnUsers" rou ON rou."userId" = u.id
        LEFT JOIN "Role" r ON r.id = rou."roleId"
        LEFT JOIN "RolesOnPermissions" rop ON rop."roleId" = r.id
        WHERE u."organizationId" = $1
        GROUP BY u.id
        LIMIT $2
        OFFSET $3;

