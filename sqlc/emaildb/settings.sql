-- name: PatchSettings :one
INSERT INTO email_settings (link_id, signature_on_replies_forwards, signature)
        VALUES ($1, COALESCE($2::bool, FALSE), $3)
        ON CONFLICT (link_id)
        DO UPDATE SET
            signature_on_replies_forwards = COALESCE($2::bool, email_settings.signature_on_replies_forwards),
            signature = COALESCE($3::text, email_settings.signature),
            updated_at = NOW()
        RETURNING link_id, signature_on_replies_forwards, signature;


-- name: FetchSettings :one
SELECT link_id, signature_on_replies_forwards, signature
        FROM email_settings
        WHERE link_id = $1;

