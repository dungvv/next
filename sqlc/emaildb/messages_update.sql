-- name: UpdateMessageReadStatus :one
UPDATE email_messages
        SET
            is_read = $1,
            updated_at = NOW()
        WHERE
            id = $2
            AND link_id = $3
        RETURNING id;


-- name: UpdateMessageReadStatusBatch :many
UPDATE email_messages
        SET
            is_read = $1,
            updated_at = NOW()
        WHERE
            "id" = ANY($2::uuid[])
            AND link_id = $3
        RETURNING id;


-- name: UpdateMessageStarredStatusBatch :many
UPDATE email_messages
        SET
            is_starred = $1,
            updated_at = NOW()
        WHERE
            "id" = ANY($2::uuid[])
            AND link_id = $3
        RETURNING id;


-- name: MarkMessageAsSent :exec
UPDATE email_messages
        SET
            provider_id = $1,
            provider_thread_id = $2,
            is_draft = false,
            is_sent = true,
            updated_at = NOW()
        WHERE
            id = $3
            AND link_id = $4;


-- name: UpdateMessageDraftStatus :exec
UPDATE email_messages
        SET
            is_draft = $1,
            updated_at = NOW()
        WHERE
            id = $2
            AND link_id = $3;

