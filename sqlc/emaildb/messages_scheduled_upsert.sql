-- name: UpsertScheduledMessage :exec
INSERT INTO email_scheduled_messages (
            link_id, message_id, send_time, sent, actor_id,
            created_at, updated_at
        )
        VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
        ON CONFLICT (link_id, message_id) DO UPDATE SET
            send_time = EXCLUDED.send_time,
            sent = EXCLUDED.sent,
            actor_id = EXCLUDED.actor_id,
            updated_at = NOW();


-- name: MarkScheduledMessageAsSent :exec
UPDATE email_scheduled_messages
        SET
            sent = true,
            updated_at = NOW()
        WHERE link_id = $1 AND message_id = $2;


-- name: ClearScheduledMessageProcessing :exec
UPDATE email_scheduled_messages
        SET
            processing = false,
            updated_at = NOW()
        WHERE link_id = $1 AND message_id = $2;

