-- name: CreateUser :one
INSERT INTO "macro_user" ("id", "username", "stripe_customer_id", "email", "has_trialed")
        VALUES ($1, $2, $3, $4, $5)
        RETURNING id;


-- name: CreateUser2 :exec
INSERT INTO "macro_user_email_verification" ("macro_user_id", "email", "is_verified")
        VALUES ($1, $2, $3);


-- name: CreateUser3 :one
INSERT INTO "User" ("id", "email", "stripeCustomerId", "organizationId", "macro_user_id")
        VALUES ($1, $2, $3, $4, $5)
        RETURNING id;


-- name: CreateUser4 :exec
INSERT INTO "RolesOnUsers" ("userId", "roleId")
            VALUES ($1, $2);


-- name: CreateUserProfile :one
INSERT INTO "User" ("id", "email", "organizationId", "macro_user_id")
        VALUES ($1, $2, $3, $4)
        RETURNING id;

