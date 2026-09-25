-- name: FetchHistoryIdForLink :one
SELECT gh.history_id
        FROM email_links l
        LEFT JOIN email_gmail_histories gh ON l.id = gh.link_id
        WHERE l.email_address = $1 AND l.provider = $2;


-- name: UpsertGmailHistory :exec
INSERT INTO email_gmail_histories (link_id, history_id, updated_at)
        VALUES ($1, $2, NOW())
        ON CONFLICT (link_id)
        DO UPDATE SET
            history_id = EXCLUDED.history_id,
            updated_at = NOW();

