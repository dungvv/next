-- name: GetDocumentNameAndType :one
SELECT
        d.name,
        d."fileType" as "file_type"
    FROM
        "Document" d
    WHERE
        d.id = $1 AND d."fileType" IS NOT NULL;

