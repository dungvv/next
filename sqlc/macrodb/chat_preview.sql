-- name: BatchGetDocumentPreviewV2 :many
SELECT
                c.id as chat_id,
                c.name as chat_name,
                c."userId" as owner,
                c."updatedAt"::timestamptz as "updated_at"
            FROM
                "Chat" c
            WHERE
                c."id" = ANY($1::text[]);

