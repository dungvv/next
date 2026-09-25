-- name: FetchSfsMapping :one
SELECT destination
        FROM email_sfs_mappings
        WHERE source = $1;


-- name: FetchSfsMappings :many
SELECT source, destination
        FROM email_sfs_mappings
        WHERE "source" = ANY($1::text[]);


-- name: InsertSfsMappings :exec
INSERT INTO email_sfs_mappings (source, destination)
        SELECT unnest($1::text[]), unnest($2::text[])
        ON CONFLICT (source) DO NOTHING;

