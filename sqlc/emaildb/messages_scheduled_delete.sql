-- name: DeleteScheduledMessage :exec
DELETE FROM email_scheduled_messages
        WHERE link_id = $1 AND message_id = $2;

