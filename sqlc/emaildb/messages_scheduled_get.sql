-- name: GetScheduledMessage :one
SELECT link_id, message_id, send_time, sent, processing, actor_id
        FROM email_scheduled_messages
        WHERE link_id = $1 AND message_id = $2;


-- name: GetAndStartProcessingScheduledMessage :one
WITH old AS (
            SELECT link_id, message_id, send_time, sent, processing, actor_id
            FROM email_scheduled_messages
            WHERE email_scheduled_messages.link_id = $1 AND email_scheduled_messages.message_id = $2
        ), updated AS (
            UPDATE email_scheduled_messages
            SET processing = true, updated_at = NOW()
            WHERE email_scheduled_messages.link_id = $1 AND email_scheduled_messages.message_id = $2
        )
        SELECT link_id, message_id, send_time, sent, processing, actor_id
        FROM old;


-- name: GetScheduledMessageNoAuth :one
SELECT link_id, message_id, send_time, sent, processing, actor_id
        FROM email_scheduled_messages
        WHERE message_id = $1 and sent = false;


-- name: FetchScheduledMessagesInBulk :many
SELECT link_id, message_id, send_time, sent, processing, actor_id
        FROM email_scheduled_messages
        WHERE "message_id" = ANY($1::uuid[]) AND sent = false;


-- name: GetScheduledDbMessagesByLinkId :many
SELECT
            m.id,
            m.provider_id,
            m.global_id,
            m.thread_id,
            m.provider_thread_id,
            m.replying_to_id,
            m.link_id,
            m.provider_history_id,
            m.internal_date_ts,
            m.snippet,
            m.size_estimate,
            m.subject,
            m.from_name,
            m.from_contact_id,
            m.sent_at,
            m.has_attachments,
            m.is_read,
            m.is_starred,
            m.is_sent,
            m.is_draft,
            m.body_text,
            m.body_html_sanitized,
            m.body_macro,
            m.headers_jsonb,
            m.created_at,
            m.updated_at
        FROM email_messages m
        JOIN email_scheduled_messages sm ON m.id = sm.message_id
        WHERE m.link_id = $1 AND sm.sent = false
        ORDER BY m.created_at DESC
        LIMIT $2 OFFSET $3;

