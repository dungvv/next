-- name: CreateAccountMergeRequest :one
INSERT INTO "account_merge_request" (id, macro_user_id, to_merge_macro_user_id, code, created_at)
        VALUES ($1, $2, $3, $4, NOW())
        RETURNING "code";

