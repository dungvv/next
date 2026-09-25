-- name: GetPaginatedThreadIdsWithMacroUserId :many
SELECT t.id, l.macro_id
        FROM email_threads t
        JOIN email_links l ON t.link_id = l.id
        ORDER BY t.latest_inbound_message_ts DESC NULLS LAST
        LIMIT $1 OFFSET $2;


-- name: GetPaginatedThreadIdsWithMacroUserIdSince :many
SELECT t.id, l.macro_id
        FROM email_threads t
        JOIN email_links l ON t.link_id = l.id
        WHERE t.updated_at >= $3
        ORDER BY t.latest_inbound_message_ts DESC NULLS LAST
        LIMIT $1 OFFSET $2;


-- name: GetThreadIdsWithMacroUserIdByIds :many
SELECT t.id, l.macro_id
        FROM email_threads t
        JOIN email_links l ON t.link_id = l.id
        WHERE t."id" = ANY($1::uuid[]);


-- name: GetLatestThreadIdsPaginated :many
SELECT t.id
        FROM email_threads t
        JOIN email_links l ON t.link_id = l.id
        WHERE l.fusionauth_user_id = $1
        ORDER BY t.latest_inbound_message_ts DESC NULLS LAST
        LIMIT $2 OFFSET $3;


-- name: GetThreadsByLinkIdAndProviderIds :many
SELECT id, provider_id as "provider_id"
        FROM email_threads
        WHERE link_id = $1
        AND "provider_id" = ANY($2::text[]);


-- name: GetThreadsByUserWithOutbound :many
SELECT t.id as thread_id
        FROM email_threads t
        JOIN email_links l ON t.link_id = l.id
        WHERE l.macro_id = $1
          AND t.latest_outbound_message_ts IS NOT NULL
        ORDER BY t.latest_outbound_message_ts DESC
        LIMIT $2 OFFSET $3;


-- name: GetOutboundThreadsByThreadIds :many
SELECT l.macro_id, t.id as thread_id
        FROM (SELECT unnest($1::text[]) AS macro_id, unnest($2::uuid[]) AS thread_id) AS inp
        JOIN email_links l ON l.macro_id = inp.macro_id
        JOIN email_threads t ON t.link_id = l.id AND t.id = inp.thread_id
        WHERE t.latest_outbound_message_ts IS NOT NULL;


-- name: GetProviderIdByLinkAndThreadId :one
SELECT provider_id
        FROM email_threads
        WHERE link_id = $1 AND id = $2;


-- name: GetMacroIdFromThreadId :one
SELECT l.macro_id
        FROM email_threads t
        JOIN email_links l ON t.link_id = l.id
        WHERE t.id = $1;


-- name: GetThreadByIdAndLinkId :one
SELECT t.id, t.provider_id, t.link_id, t.inbox_visible, t.is_read,
               t.latest_inbound_message_ts, t.latest_outbound_message_ts,
               t.latest_non_spam_message_ts, t.created_at, t.updated_at
        FROM email_threads t
        WHERE t.id = $1 AND t.link_id = $2;


-- name: GetAllThreadIdsPaginated :many
SELECT
            id as "thread_id"
        FROM
            email_threads
        ORDER BY
            created_at ASC
        LIMIT $1
        OFFSET $2;


-- name: GetThreadIdsByContactIds :many
SELECT DISTINCT m.thread_id as "thread_id"
        FROM email_messages m
        WHERE m."from_contact_id" = ANY($1::uuid[])
        UNION
        SELECT DISTINCT m.thread_id as "thread_id"
        FROM email_messages m
        JOIN email_message_recipients mr ON m.id = mr.message_id
        WHERE mr."contact_id" = ANY($1::uuid[]);

