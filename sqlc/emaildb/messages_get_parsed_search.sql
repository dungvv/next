-- name: GetParsedSearchMessageById :one
SELECT
            m.id, m.provider_id, m.global_id, m.link_id, m.thread_id, m.provider_thread_id, m.provider_history_id,
            m.replying_to_id, m.internal_date_ts, m.snippet, m.size_estimate, m.subject, m.from_name,
            m.from_contact_id, m.sent_at, m.has_attachments, m.is_read, m.is_starred, m.is_sent, m.is_draft,
            m.body_text as body_text,
            m.body_html_sanitized as body_html_sanitized,
            NULL::TEXT as body_macro,
            m.headers_jsonb, m.created_at, m.updated_at
        FROM email_messages m
        JOIN email_links l ON m.link_id = l.id
        WHERE m.id = $1;


-- name: GetParsedSearchMessagesByThreadIds :many
SELECT
            id,
            provider_id,
            global_id,
            thread_id,
            provider_thread_id,
            replying_to_id,
            link_id,
            provider_history_id,
            internal_date_ts,
            snippet,
            size_estimate,
            subject,
            from_name,
            from_contact_id,
            sent_at,
            has_attachments,
            is_read,
            is_starred,
            is_sent,
            is_draft,
            headers_jsonb,
            created_at,
            updated_at,
            body_text as body_text,
            body_html_sanitized as body_html_sanitized,
            NULL::TEXT as body_macro
        FROM email_messages
        WHERE "thread_id" = ANY($1::uuid[])
        ORDER BY internal_date_ts DESC NULLS LAST;

