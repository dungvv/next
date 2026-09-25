-- name: DoesDocumentExist :one
SELECT
            d.id
        FROM
            "Document" d
        WHERE
            d.id = $1;

