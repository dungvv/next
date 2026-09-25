-- name: DeleteAccountMergeRequest :exec
DELETE FROM "account_merge_request"
        WHERE "id" = $1;

