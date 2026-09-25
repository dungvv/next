-- name: MergeAccounts :exec
UPDATE "User" SET "macro_user_id" = $1 WHERE "macro_user_id" = $2;


-- name: MergeAccounts2 :exec
UPDATE "macro_user_email_verification" SET "macro_user_id" = $1 WHERE "macro_user_id" = $2;


-- name: MergeAccounts3 :exec
DELETE FROM "macro_user" WHERE "id" = $1;

