-- name: SetDeletedAt :exec
UPDATE email_links_history
        SET deleted_at = NOW(),
            deletion_reason = $2
        WHERE link_id = $1
          AND deleted_at IS NULL;

