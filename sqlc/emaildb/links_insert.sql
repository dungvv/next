-- name: UpsertLink :one
INSERT INTO email_links (id, macro_id, fusionauth_user_id, email_address, provider, is_sync_active)
        VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (fusionauth_user_id, email_address, provider)
        DO UPDATE SET
            is_sync_active = EXCLUDED.is_sync_active,
            needs_reauth = false,
            last_sync_error_at = NULL,
            updated_at = NOW()
        RETURNING id, is_primary;


-- name: UpsertLink2 :exec
INSERT INTO email_settings (link_id) -- default settings for new links
        VALUES ($1)
        ON CONFLICT (link_id)
        DO NOTHING;

