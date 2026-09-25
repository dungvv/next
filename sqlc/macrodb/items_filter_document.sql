-- name: FilterDocumentsByFileTypes :many
SELECT
            d.id
        FROM
            "Document" d
        WHERE
            d."id" = ANY($1::text[])
            AND d."fileType" = ANY($2::text[]);

