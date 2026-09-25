-- name: GetAllDocuments :one
SELECT COUNT(*) as "count"
        FROM "Document" d
        WHERE d."deletedAt" IS NULL;


-- name: GetAllDocuments2 :many
SELECT
            d.id as document_id,
            d.owner as owner,
            d.name as document_name,
            COALESCE(db.id, di.id) as "document_version_id",
            d."branchedFromId" as "branched_from_id",
            d."branchedFromVersionId" as "branched_from_version_id",
            d."documentFamilyId" as "document_family_id",
            d."fileType" as file_type,
            d."createdAt"::timestamptz as created_at,
            d."updatedAt"::timestamptz as updated_at,
            d."deletedAt"::timestamptz as deleted_at,
            db.bom_parts as "document_bom",
            di.modification_data as "modification_data",
            d."projectId" as "project_id",
            p.name as "project_name",
            di.sha as "sha",
            dt.sub_type as "sub_type"
        FROM
            "Document" d
        LEFT JOIN document_sub_type dt ON dt.document_id = d.id
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
        ) db ON d."fileType" = 'docx'
        LEFT JOIN LATERAL (
            SELECT
                i.id,
                i."documentId",
                i."sha",
                i."createdAt",
                (
                    SELECT
                        imod."modificationData"
                    FROM
                        "DocumentInstanceModificationData" imod
                    WHERE
                        imod."documentInstanceId" = i.id
                ) as modification_data,
                i."updatedAt"
            FROM
                "DocumentInstance" i
            WHERE
                i."documentId" = d.id
            ORDER BY
                i."updatedAt" DESC
            LIMIT 1
        ) di ON d."fileType" IS DISTINCT FROM 'docx'
        LEFT JOIN "Project" p ON p.id = d."projectId"
        WHERE
        d."deletedAt" IS NULL
        ORDER BY d."createdAt" DESC
        LIMIT $1 OFFSET $2;


-- name: GetDocumentsToDelete :many
SELECT d.id
            FROM "Document" d
            WHERE d."deletedAt" IS NOT NULL AND d."deletedAt" <= $1;


-- name: GetAllDocumentIdsPaginated :many
SELECT
            id as "document_id"
        FROM
            "Document"
        WHERE
            "deletedAt" IS NULL
        ORDER BY
            "createdAt" ASC
        LIMIT $1
        OFFSET $2;

