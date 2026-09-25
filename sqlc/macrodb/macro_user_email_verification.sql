-- name: GetMacroUserEmailVerification :one
SELECT "is_verified"
        FROM "macro_user_email_verification"
        WHERE "email" = $1;


-- name: UpsertMacroUserEmailVerification :exec
INSERT INTO "macro_user_email_verification" ("macro_user_id", "email", "is_verified")
            VALUES ($1, $2, $3)
        ON CONFLICT ("email") DO UPDATE SET "macro_user_id" = $1, "is_verified" = $3;

