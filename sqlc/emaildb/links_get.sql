-- name: FetchLinkByEmail :one
SELECT id, macro_id, fusionauth_user_id, email_address, provider as "provider",
               is_sync_active, is_primary, needs_reauth, last_sync_error_at, created_at, updated_at
        FROM email_links
        WHERE email_address = $1 AND provider = $2
        LIMIT 1;


-- name: FetchLinkByMacroId :one
SELECT id, macro_id, fusionauth_user_id, email_address, provider as "provider",
               is_sync_active, is_primary, needs_reauth, last_sync_error_at, created_at, updated_at
        FROM email_links
        WHERE macro_id = $1
        ORDER BY created_at DESC
        LIMIT 1;


-- name: FetchLinkByMacroIdAndEmailAddress :one
SELECT id, macro_id, fusionauth_user_id, email_address, provider as "provider",
               is_sync_active, is_primary, needs_reauth, last_sync_error_at, created_at, updated_at
        FROM email_links
        WHERE macro_id = $1 AND LOWER(email_address) = $2
        ORDER BY created_at DESC
        LIMIT 1;


-- name: FetchInboxesForMacroId :many
SELECT id as "id", macro_id as "macro_id",
               fusionauth_user_id as "fusionauth_user_id",
               email_address as "email_address",
               provider as "provider",
               is_sync_active as "is_sync_active",
               is_primary as "is_primary",
               needs_reauth as "needs_reauth",
               last_sync_error_at,
               created_at as "created_at",
               updated_at as "updated_at"
        FROM (
            SELECT el.id, el.macro_id, el.fusionauth_user_id, el.email_address,
                   el.provider, el.is_sync_active, el.is_primary, el.needs_reauth,
                   el.last_sync_error_at, el.created_at, el.updated_at
            FROM email_links el
            WHERE el.macro_id = $1
            UNION
            SELECT el.id, el.macro_id, el.fusionauth_user_id, el.email_address,
                   el.provider, el.is_sync_active, el.is_primary, el.needs_reauth,
                   el.last_sync_error_at, el.created_at, el.updated_at
            FROM email_links el
            JOIN macro_user_links mul ON el.id = mul.link_id
            WHERE mul.primary_macro_id = $1
        ) AS combined
        ORDER BY created_at DESC;


-- name: FetchInboxDetailsForMacroId :many
SELECT l.id as "id", l.macro_id as "macro_id",
               l.fusionauth_user_id as "fusionauth_user_id",
               l.email_address as "email_address",
               l.provider as "provider",
               l.is_sync_active as "is_sync_active",
               l.is_primary as "is_primary",
               l.needs_reauth as "needs_reauth",
               l.last_sync_error_at,
               l.created_at as "created_at",
               l.updated_at as "updated_at",
               s.signature_on_replies_forwards as "signature_on_replies_forwards",
               s.signature,
               bj.status as "latest_backfill_status",
               c.sfs_photo_url as "photo_url",
               COALESCE(g.granted_scopes, '{}') AS "google_granted_scopes",
               (g.calendar_disabled_at IS NOT NULL)::bool AS "calendar_disabled",
               EXISTS (
                   SELECT 1 FROM calendar_accounts ca WHERE ca.email_link_id = l.id
               ) AS "has_calendar_data"
        FROM (
            SELECT el.id, el.macro_id, el.fusionauth_user_id, el.email_address,
                   el.provider, el.is_sync_active, el.is_primary, el.needs_reauth,
                   el.last_sync_error_at, el.created_at, el.updated_at
            FROM email_links el
            WHERE el.macro_id = $1
            UNION
            SELECT el.id, el.macro_id, el.fusionauth_user_id, el.email_address,
                   el.provider, el.is_sync_active, el.is_primary, el.needs_reauth,
                   el.last_sync_error_at, el.created_at, el.updated_at
            FROM email_links el
            JOIN macro_user_links mul ON el.id = mul.link_id
            WHERE mul.primary_macro_id = $1
        ) l
        LEFT JOIN email_link_google_scopes g ON g.link_id = l.id
        LEFT JOIN email_settings s ON s.link_id = l.id
        LEFT JOIN LATERAL (
            SELECT status FROM email_backfill_jobs
            WHERE link_id = l.id
            ORDER BY created_at DESC
            LIMIT 1
        ) bj ON true
        LEFT JOIN email_contacts c
            ON c.link_id = l.id AND LOWER(c.email_address) = LOWER(l.email_address)
        ORDER BY l.created_at DESC;


-- name: FetchLinksByFusionauthUserId :many
SELECT id, fusionauth_user_id, macro_id, email_address, provider as "provider",
               is_sync_active, is_primary, needs_reauth, last_sync_error_at, created_at, updated_at
        FROM email_links
        WHERE fusionauth_user_id = $1
        ORDER BY created_at DESC;


-- name: FetchOwnedLinkForThread :one
SELECT l.id, l.macro_id, l.fusionauth_user_id, l.email_address, l.provider as "provider",
               l.is_sync_active, l.is_primary, l.needs_reauth, l.last_sync_error_at,
               l.created_at, l.updated_at
        FROM email_threads t
        JOIN email_links l ON l.id = t.link_id
        WHERE t.id = $1
          AND (
              l.macro_id = $2
              OR EXISTS (
                  SELECT 1 FROM macro_user_links mul
                  WHERE mul.link_id = l.id AND mul.primary_macro_id = $2
              )
          );


-- name: FetchOwnedLinkForMessage :one
SELECT l.id, l.macro_id, l.fusionauth_user_id, l.email_address, l.provider as "provider",
               l.is_sync_active, l.is_primary, l.needs_reauth, l.last_sync_error_at,
               l.created_at, l.updated_at
        FROM email_messages m
        JOIN email_links l ON l.id = m.link_id
        WHERE m.id = $1
          AND (
              l.macro_id = $2
              OR EXISTS (
                  SELECT 1 FROM macro_user_links mul
                  WHERE mul.link_id = l.id AND mul.primary_macro_id = $2
              )
          );


-- name: FetchLinkById :one
SELECT id, macro_id, fusionauth_user_id, email_address, provider as "provider",
               is_sync_active, is_primary, needs_reauth, last_sync_error_at, created_at, updated_at
        FROM email_links
        WHERE id = $1;

