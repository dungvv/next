-- name: DeleteLinkById :exec
DELETE FROM email_links
        WHERE id = $1;

