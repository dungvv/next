-- name: GetDocument :one
SELECT
            d.id,
            d.name,
            d."owner",
            d."fileType" as "file_type",
            di.id as "document_version_id"
        FROM
            "Document" d
        LEFT JOIN LATERAL (
            SELECT
                i.id
            FROM
                "DocumentInstance" i
            WHERE
                i."documentId" = d.id
            ORDER BY
                i."createdAt" ASC
            LIMIT 1
        ) di ON true
        WHERE
            d."deletedAt" IS NULL AND
            d.id = $1 AND
            d."fileType" IS NOT NULL;

