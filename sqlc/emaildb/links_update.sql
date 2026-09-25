-- name: UpdateLinkFusionauthUserId :exec
UPDATE email_links
        SET fusionauth_user_id = $2, updated_at = NOW()
        WHERE id = $1;


-- name: SetLinkNeedsReauth :one
WITH prev AS (
            SELECT needs_reauth FROM email_links WHERE id = $1 FOR UPDATE
        )
        UPDATE email_links e
        SET needs_reauth = true,
            last_sync_error_at = NOW(),
            updated_at = NOW()
        FROM prev
        WHERE e.id = $1
        RETURNING (NOT prev.needs_reauth) AS "did_transition";


-- name: ClearLinkNeedsReauth :exec
UPDATE email_links
        SET needs_reauth = false,
            last_sync_error_at = NULL,
            updated_at = NOW()
        WHERE id = $1 AND needs_reauth = true;


-- name: UpdateLinkSyncStatus :exec
UPDATE email_links
        SET is_sync_active = $2, updated_at = NOW()
        WHERE id = $1 AND is_sync_active IS DISTINCT FROM $2;

