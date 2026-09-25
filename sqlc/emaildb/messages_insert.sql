-- name: InsertDbMessage :one
INSERT INTO email_messages (
        id, provider_id, link_id, global_id, thread_id, provider_thread_id, replying_to_id, provider_history_id, internal_date_ts,
            snippet, size_estimate, subject, from_name, from_contact_id, sent_at, has_attachments, is_read,
            is_starred, is_sent, is_draft, body_text, body_html_sanitized, headers_jsonb
        )
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
        ON CONFLICT (link_id, provider_id) WHERE provider_id IS NOT NULL DO UPDATE SET
            global_id = EXCLUDED.global_id,
            provider_history_id = EXCLUDED.provider_history_id,
            provider_thread_id = EXCLUDED.provider_thread_id,
            replying_to_id = EXCLUDED.replying_to_id,
            from_name = EXCLUDED.from_name,
            from_contact_id = EXCLUDED.from_contact_id,
            internal_date_ts = EXCLUDED.internal_date_ts,
            snippet = EXCLUDED.snippet,
            size_estimate = EXCLUDED.size_estimate,
            subject = EXCLUDED.subject,
            sent_at = EXCLUDED.sent_at,
            has_attachments = EXCLUDED.has_attachments,
            is_read = EXCLUDED.is_read,
            is_starred = EXCLUDED.is_starred,
            is_sent = EXCLUDED.is_sent,
            is_draft = EXCLUDED.is_draft,
            headers_jsonb = EXCLUDED.headers_jsonb,
            body_text = EXCLUDED.body_text,
            body_html_sanitized = EXCLUDED.body_html_sanitized,
            updated_at = NOW()
        RETURNING id;

