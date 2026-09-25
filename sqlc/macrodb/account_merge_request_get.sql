-- name: GetMergeRequestInfo :one
SELECT
            "id" as "merge_request_id",
            "to_merge_macro_user_id" as "to_merge_macro_user_id"
        FROM "account_merge_request"
        WHERE "code" = $1 AND "macro_user_id" = $2;


-- name: CheckMergeRequestForToMergeMacroUserId :one
SELECT
            id
        FROM account_merge_request
        WHERE to_merge_macro_user_id = $1;

