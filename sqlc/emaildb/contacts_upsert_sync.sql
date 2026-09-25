-- name: UpsertContacts :many
SELECT id, email_address, name
        FROM email_contacts
        WHERE (link_id, email_address) IN (
            SELECT unnest($1::uuid[]), unnest($2::varchar[])
        );


-- name: UpsertContacts2 :exec
INSERT INTO email_contacts (id, link_id, email_address, name, original_photo_url, sfs_photo_url, updated_at)
    SELECT u.c1, u.c2, u.c3, u.c4, u.c5, u.c6, NOW()
    FROM (SELECT unnest($1::uuid[]) AS c1, unnest($2::uuid[]) AS c2,
                 unnest($3::varchar[]) AS c3, unnest($4::varchar[]) AS c4,
                 unnest($5::text[]) AS c5, unnest($6::text[]) AS c6) u
    ON CONFLICT (link_id, email_address)
    DO UPDATE SET
        -- Overwrite existing name - contact names take precedence over names included with emails
        name = COALESCE(EXCLUDED.name, email_contacts.name),
        original_photo_url = COALESCE(EXCLUDED.original_photo_url, email_contacts.original_photo_url),
        sfs_photo_url = COALESCE(EXCLUDED.sfs_photo_url, email_contacts.sfs_photo_url),
        updated_at = NOW();

