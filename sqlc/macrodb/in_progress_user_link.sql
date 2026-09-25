-- name: CountExistingInProgressUserLinksForUser :one
SELECT
                COUNT(id) as count
            FROM
                in_progress_user_link
            WHERE
                macro_user_id = $1
                AND created_at > $2;


-- name: CreateInProgressUserLink :exec
INSERT INTO in_progress_user_link (id, macro_user_id)
            VALUES ($1, $2);


-- name: CreateInProgressGoogleLink :exec
INSERT INTO in_progress_user_link (
                id,
                macro_user_id,
                requested_google_scopes
            )
            VALUES ($1, $2, $3);


-- name: DeleteInProgressUserLink :exec
DELETE FROM
                in_progress_user_link
            WHERE
                id = $1;


-- name: DeleteDayOldInProgressUserLinks :exec
DELETE FROM
                in_progress_user_link
            WHERE
                created_at < $1;


-- name: GetMacroUserIdByLinkId :one
SELECT
                id,
                macro_user_id
            FROM
                in_progress_user_link
            WHERE
                id = $1;


-- name: GetInProgressUserLink :one
SELECT
                macro_user_id,
                linked_email,
                requested_google_scopes,
                granted_google_scopes
            FROM
                in_progress_user_link
            WHERE
                id = $1;


-- name: SetLinkedGoogleGrant :exec
UPDATE in_progress_user_link
            SET linked_email = $1,
                granted_google_scopes = $2
            WHERE id = $3;


-- name: SetLinkedEmail :exec
UPDATE in_progress_user_link
            SET linked_email = $1
            WHERE id = $2;

