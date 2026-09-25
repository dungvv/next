-- name: GetSimpleMessageByProviderAndLink :one
SELECT
            m.id, m.provider_id, m.global_id, m.link_id, m.thread_id, m.provider_thread_id, m.replying_to_id, 
            m.provider_history_id, m.internal_date_ts, m.snippet, m.size_estimate, m.subject, m.from_name,
            m.from_contact_id, m.sent_at, m.has_attachments, m.is_read, m.is_starred, m.is_sent, m.is_draft,
            NULL::TEXT as body_text,
            NULL::TEXT as body_html_sanitized,
            NULL::TEXT as body_macro,
            m.headers_jsonb, m.created_at, m.updated_at
        FROM email_messages m
        WHERE m.provider_id = $1 AND m.link_id = $2;


-- name: GetSimpleMessage :one
SELECT
            m.id, m.provider_id, m.global_id, m.link_id, m.thread_id, m.provider_thread_id, m.replying_to_id,
            m.provider_history_id, m.internal_date_ts, m.snippet, m.size_estimate, m.subject, m.from_name,
            m.from_contact_id, m.sent_at, m.has_attachments, m.is_read, m.is_starred, m.is_sent, m.is_draft,
            NULL::TEXT as body_text,
            NULL::TEXT as body_html_sanitized,
            NULL::TEXT as body_macro,
            m.headers_jsonb, m.created_at, m.updated_at
        FROM email_messages m
        JOIN email_links l ON m.link_id = l.id
        WHERE m.id = $1 AND l.fusionauth_user_id = $2;


-- name: GetSimpleMessagesBatch :many
SELECT
            m.id, m.provider_id, m.global_id, m.link_id, m.thread_id, m.provider_thread_id, m.replying_to_id,
            m.provider_history_id, m.internal_date_ts, m.snippet, m.size_estimate, m.subject, m.from_name,
            m.from_contact_id, m.sent_at, m.has_attachments, m.is_read, m.is_starred, m.is_sent, m.is_draft,
            NULL::TEXT as body_text,
            NULL::TEXT as body_html_sanitized,
            NULL::TEXT as body_macro,
            m.headers_jsonb, m.created_at, m.updated_at
        FROM email_messages m
        JOIN email_links l ON m.link_id = l.id
        WHERE m."id" = ANY($1::uuid[]) AND l.fusionauth_user_id = $2;


-- name: GetSimpleMessagesForThread :many
SELECT
            m.id,
            m.provider_id,
            m.global_id,
            m.link_id,
            m.thread_id,
            m.provider_thread_id,
            m.replying_to_id,
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
            NULL::TEXT as body_text,
            NULL::TEXT as body_html_sanitized,
            NULL::TEXT as body_macro,
            m.headers_jsonb,
            m.created_at,
            m.updated_at
        FROM
            email_messages m
        WHERE
            m.thread_id = $1 AND m.link_id = $2
        ORDER BY
            m.internal_date_ts DESC NULLS LAST;


-- name: GetFirstSimpleMessageDraft :one
SELECT
            m.id, m.provider_id, m.global_id, m.link_id, m.thread_id, m.provider_thread_id, m.provider_history_id,
            m.replying_to_id, m.internal_date_ts, m.snippet, m.size_estimate, m.subject, m.from_name,
            m.from_contact_id, m.sent_at, m.has_attachments, m.is_read, m.is_starred, m.is_sent, m.is_draft,
            NULL::TEXT as body_text,
            NULL::TEXT as body_html_sanitized,
            NULL::TEXT as body_macro,
            m.headers_jsonb, m.created_at, m.updated_at
        FROM email_messages m
        WHERE m.link_id = $1
          AND m.is_draft = true
          AND jsonb_path_exists(
              m.headers_jsonb,
              '$[*] ? (@."Macro-In-Reply-To" == $macro_uuid)'::jsonpath,
              jsonb_build_object('macro_uuid', $2::text)
          )
        ORDER BY m.created_at DESC
        LIMIT 1;

