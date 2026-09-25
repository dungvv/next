-- name: GetDocumentsForSearch :many
SELECT
            d.id as document_id,
            d.owner as owner,
            d."fileType" as "file_type",
            COALESCE(db.id, di.id, dipdf.id) as "document_version_id",
            d."updatedAt"::timestamptz as "updated_at"
        FROM
            "Document" d
        LEFT JOIN document_sub_type dst ON dst.document_id = d.id
        LEFT JOIN LATERAL (
            SELECT
                b.id
            FROM
                "DocumentBom" b
            WHERE
                b."documentId" = d.id
            ORDER BY
                b."createdAt" DESC
            LIMIT 1
        ) db ON d."fileType" = 'docx'
        LEFT JOIN LATERAL (
            SELECT
                i.id
            FROM
                "DocumentInstance" i
            WHERE
                i."documentId" = d.id
            ORDER BY
                i."updatedAt" ASC
            LIMIT 1
        ) dipdf ON d."fileType" = 'pdf'
        LEFT JOIN LATERAL (
            SELECT
                i.id
            FROM
                "DocumentInstance" i
            WHERE
                i."documentId" = d.id
            ORDER BY
                i."createdAt" DESC
            LIMIT 1
        ) di ON d."fileType" IS DISTINCT FROM 'docx' AND d."fileType" IS DISTINCT FROM 'pdf'
        WHERE
            d."fileType" IS NOT NULL
            AND ($3::text[] IS NULL OR d."fileType" = ANY($3::text[]))
            AND ($4::text IS NULL OR dst.sub_type::text = $4)
            AND ($5::timestamptz IS NULL OR d."updatedAt" >= $5)
            AND ($6::timestamptz IS NULL OR d."updatedAt" < $6)
            AND (
                $7::bool IS NULL
                OR ($7 AND d."deletedAt" IS NOT NULL)
                OR (NOT $7 AND d."deletedAt" IS NULL)
            )
            AND (
                $2::timestamptz IS NULL
                OR (d."updatedAt", d.id) > ($2, $8::text)
            )
        ORDER BY d."updatedAt" ASC, d.id ASC
        LIMIT $1;

