-- name: BatchVerify :many
SELECT d."documentId" as "document_id"
            FROM "DocumentText" as d
            WHERE d."documentId" = ANY($1::text[])
            AND d."tokenCount" > 0;

