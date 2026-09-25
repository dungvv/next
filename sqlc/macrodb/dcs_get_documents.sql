-- name: GetDocumentsCount :one
SELECT COUNT(*) as "count"
            FROM "Document" d
            WHERE d."deletedAt" IS NULL;


-- name: GetPaginatedDocuments :many
SELECT
            d.id,
            d."owner",
            d.name,
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
            d."deletedAt" IS NULL
            AND d."fileType" IS NOT NULL
        ORDER BY d."createdAt" DESC
        LIMIT $1 OFFSET $2;


-- name: GetDocumentsWithoutTextCount :one
SELECT COUNT(*) as "count"
            FROM "Document" d
            LEFT JOIN "DocumentText" dt ON d.id = dt."documentId"
            WHERE d."deletedAt" IS NULL
            AND dt."documentId" IS NULL;


-- name: GetPaginatedDocumentsWithoutText :many
SELECT
            d.id,
            d.name,
            d.owner,
            d."fileType" as "file_type",
            di.id as "document_version_id"
        FROM
            "Document" d
        LEFT JOIN "DocumentText" dt ON d.id = dt."documentId"
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
            d."deletedAt" IS NULL
            AND dt."documentId" IS NULL
            AND d."fileType" IS NOT NULL
        ORDER BY d."createdAt" DESC
        LIMIT $1 OFFSET $2;

