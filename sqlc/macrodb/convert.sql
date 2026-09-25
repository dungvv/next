-- name: GetDocxFiles :one
SELECT COUNT(*) as "count"
        FROM "Document" d
        WHERE d."fileType" = 'docx' AND d."deletedAt" IS NULL AND d.uploaded = true;


-- name: GetDocxFiles2 :many
SELECT
            d.id as document_id,
            d.owner as owner,
            d.name as document_name,
            db.id as "document_version_id",
            d."fileType" as "file_type",
            d."createdAt"::timestamptz as created_at,
            d."updatedAt"::timestamptz as updated_at,
            db.bom_parts as "document_bom",
            d."projectId" as "project_id",
            p.name as "project_name"
        FROM
            "Document" d
        LEFT JOIN LATERAL (
            SELECT
                b.id,
                (
                    SELECT
                        json_agg(
                            json_build_object(
                                'id', bp.id,
                                'sha', bp.sha,
                                'path', bp.path
                            )
                        )
                    FROM
                        "BomPart" bp
                    WHERE
                        bp."documentBomId" = b.id
                ) as bom_parts
            FROM
                "DocumentBom" b
            WHERE
                b."documentId" = d.id
            ORDER BY
                b."createdAt" DESC
            LIMIT 1

        ) db ON true
        LEFT JOIN LATERAL (
            SELECT
                p.name
            FROM "Project" p
            WHERE p.id = d."projectId"
        ) p ON d."projectId" IS NOT NULL
        WHERE d."fileType" = 'docx' AND d."deletedAt" IS NULL AND d.uploaded = true
        ORDER BY d."updatedAt" DESC
        LIMIT $1 OFFSET $2;

