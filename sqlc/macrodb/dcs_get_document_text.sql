-- name: GetPdfDocxDocumentText :one
SELECT
            d."documentId" as "document_id",
            d.content as "content",
            d."tokenCount" as "token_count"
        FROM
            "DocumentText" d
        WHERE
            d."documentId" = $1;


-- name: GetDocumentTextsWithNoTokens :many
SELECT
            d."documentId" as "document_id"
        FROM
            "DocumentText" d
        WHERE
            d."tokenCount" = 0;


-- name: DocumentTextExtractionStatus :one
SELECT
            LENGTH(REGEXP_REPLACE(d."content", '\s', '', 'g')) AS content_length
        FROM
            "DocumentText" d
        WHERE
            d."documentId" = $1;


-- name: BatchDocumentTextExtractionStatus :many
SELECT DISTINCT
            d."documentId",
            LENGTH(REGEXP_REPLACE(d."content", '\s', '', 'g')) AS content_length
        FROM
            "DocumentText" d
        WHERE
            d."documentId" = ANY($1::text[]);


-- name: GetPdfDocxTokenCount :one
SELECT
            "tokenCount" as token_count
            FROM
            "DocumentText"
            WHERE
            "documentId" = $1;

