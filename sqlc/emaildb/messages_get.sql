-- name: GetMessageSenderAndPrettySender :many
SELECT
            m.id as "message_id",
            c.email_address as "sender",
            COALESCE(c.name, c.email_address) as "pretty_sender"
        FROM email_messages m
        LEFT JOIN email_contacts c ON c.id = m.from_contact_id
        WHERE m."id" = ANY($1::uuid[])
        AND m."link_id" = ANY($2::uuid[]);


-- name: GetMessageAndThreadIdByProviderId :one
SELECT id, thread_id
        FROM email_messages
        WHERE link_id = $1 AND provider_id = $2
        LIMIT 1;


-- name: FilterExistingProviderMessageIds :many
SELECT provider_id AS "provider_id"
        FROM email_messages
        WHERE link_id = $1 AND "provider_id" = ANY($2::text[]);


-- name: MessageExistsByProviderId :one
SELECT EXISTS(
            SELECT 1
            FROM email_messages
            WHERE provider_id = $1 AND link_id = $2
        ) AS "exists";


-- name: FetchMessagesMetadata :many
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
            -- No body attributes
            NULL::text as "body_text",
            NULL::text as "body_html_sanitized",
            NULL::text as "body_macro"
        FROM email_messages
        WHERE thread_id = $1
        ORDER BY internal_date_ts DESC NULLS LAST;


-- name: FetchMessagesWithLabels :many
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
            -- No body attributes
            NULL::text as "body_text",
            NULL::text as "body_html_sanitized",
            NULL::text as "body_macro"
        FROM email_messages
        WHERE thread_id = $1 and link_id = $2
        ORDER BY internal_date_ts DESC NULLS LAST;


-- name: GetMessageThreadingHeaders :one
SELECT 
          -- Use trim() to remove the leading/trailing angle brackets
          trim(jsonb_path_query_first(headers_jsonb, '$[*] ? (@.name like_regex "message-id" flag "i").value') #>> '{}', '<>') as "message_id",
          -- The references header can have multiple IDs, so we just return it raw
          (jsonb_path_query_first(headers_jsonb, '$[*] ? (@.name like_regex "references" flag "i").value') #>> '{}')::text as "references"
        FROM email_messages
        WHERE id = $1 AND link_id = $2;


-- name: GetMessageIdByGlobalId :one
SELECT id
        FROM email_messages
        WHERE link_id = $1 AND global_id = $2;


-- name: GetMessageToSend :one
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
            body_text,
            body_html_sanitized,
            body_macro,
            headers_jsonb,
            created_at,
            updated_at
        FROM email_messages
        WHERE id = $1 and link_id = $2;


-- name: DraftExistsWithId :one
SELECT EXISTS(
                SELECT 1
                FROM email_messages
                WHERE id = $1 AND link_id = $2 AND is_draft = true
            ) as "exists";

