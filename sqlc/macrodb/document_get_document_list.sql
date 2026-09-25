-- name: GetDocumentList :many
SELECT
            d.id as "document_id",
            COALESCE(db.id, di.id) as "document_version_id",
            d.name as "document_name",
            d."fileType" as "file_type",
            d."branchedFromId" as branched_from_id,
            d."branchedFromVersionId" as branched_from_version_id,
            d."documentFamilyId" as document_family_id,
            d."createdAt"::timestamptz as created_at,
            d."updatedAt"::timestamptz as updated_at
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
                i."createdAt" DESC
            LIMIT 1
        ) di ON d."fileType" IS DISTINCT FROM 'docx'
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
        WHERE
            d.owner = $1 AND d."deletedAt" IS NULL;

