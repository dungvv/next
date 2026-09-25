-- name: GetUserDocumentIds :many
SELECT
                    d.id as document_id
                FROM
                    "Document" d
                WHERE
                    d.owner = $1 AND d."deletedAt" IS NULL AND d."fileType" = ANY($2::text[]);


-- name: GetUserDocumentIds2 :many
SELECT
                d.id as document_id
            FROM
                "Document" d
            WHERE
                d.owner = $1 AND d."deletedAt" IS NULL;

