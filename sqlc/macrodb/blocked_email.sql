-- name: BulkUpsertBlockEmail :exec
INSERT INTO "BlockedEmail" (email)
            SELECT email FROM unnest($1::text[]) AS email
            ON CONFLICT (email) DO NOTHING;


-- name: UpsertBlockEmail :exec
INSERT INTO "BlockedEmail" (email)
            VALUES ($1)
            ON CONFLICT (email) DO NOTHING;


-- name: GetBlockedEmails :many
SELECT email
            FROM "BlockedEmail"
            WHERE "email" = ANY($1::text[]);

