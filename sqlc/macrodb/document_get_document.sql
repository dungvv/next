-- name: GetDocumentName :one
SELECT 
            d.name 
        FROM "Document" d
        WHERE d."id" = $1;


-- name: GetLatestDocumentVersionId :one
SELECT 
            di.id, 
            d.uploaded
        FROM "DocumentInstance" di
        JOIN "Document" d ON di."documentId" = d.id
        WHERE di."documentId" = $1
        ORDER BY di."createdAt" DESC
        LIMIT 1;


-- name: GetLatestDocumentBomVersionId :one
SELECT 
            db.id
        FROM "DocumentBom" db
        JOIN "Document" d ON db."documentId" = d.id
        WHERE db."documentId" = $1
        ORDER BY db."createdAt" DESC
        LIMIT 1;


-- name: GetDocumentVersionId :one
SELECT
            COALESCE(db.id, di.id) as "id",
            d.uploaded
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
        ) di ON d."fileType" IS DISTINCT FROM 'docx'
        LEFT JOIN LATERAL (
            SELECT
                b.id
            FROM
                "DocumentBom" b
            WHERE
                b."documentId" = d.id
            ORDER BY
                b."createdAt" ASC
            LIMIT 1
        ) db ON d."fileType" = 'docx'
        WHERE
            d.id = $1
        LIMIT 1;


-- name: GetBasicDocument :one
SELECT
            d.id as "document_id",
            d.owner,
            d.name as "document_name",
            d."branchedFromId" as "branched_from_id",
            d."branchedFromVersionId" as "branched_from_version_id",
            d."documentFamilyId" as "document_family_id",
            d."fileType" as "file_type",
            dt.sub_type as "sub_type",
            d."projectId" as "project_id",
            d."deletedAt"::timestamptz as "deleted_at"
        FROM
            "Document" d
        LEFT JOIN document_sub_type dt ON dt.document_id = d.id
        WHERE
            d.id = $1
        LIMIT 1;


-- name: GetBasicDocuments :many
SELECT
            d.id as "document_id",
            d.owner,
            d.name as "document_name",
            d."branchedFromId" as "branched_from_id",
            d."branchedFromVersionId" as "branched_from_version_id",
            d."documentFamilyId" as "document_family_id",
            d."fileType" as "file_type",
            dt.sub_type as "sub_type",
            d."projectId" as "project_id",
            d."deletedAt"::timestamptz as "deleted_at"
        FROM
            "Document" d
        LEFT JOIN document_sub_type dt ON dt.document_id = d.id
        WHERE
            d."id" = ANY($1::text[]);


-- name: GetDeletedDocumentInfo :one
SELECT
            d.id as "document_id",
            d.owner,
            d.name as "document_name",
            d."branchedFromId" as "branched_from_id",
            d."branchedFromVersionId" as "branched_from_version_id",
            d."documentFamilyId" as "document_family_id",
            d."fileType" as "file_type",
            dt.sub_type as "sub_type",
            d."projectId" as "project_id",
            d."deletedAt"::timestamptz as "deleted_at"
        FROM
            "Document" d
        LEFT JOIN document_sub_type dt ON dt.document_id = d.id
        WHERE
            d.id = $1;


-- name: GetDocument2 :one
SELECT
            d.id as "document_id",
            d.owner as "owner",
            COALESCE(db.id, di.id) as "document_version_id",
            d.name as "document_name",
            d."branchedFromId" as "branched_from_id",
            d."branchedFromVersionId" as "branched_from_version_id",
            d."documentFamilyId" as "document_family_id",
            d."createdAt"::timestamptz as "created_at",
            d."updatedAt"::timestamptz as "updated_at",
            d."fileType" as "file_type",
            db.bom_parts as "document_bom",
            di.modification_data as "modification_data",
            d."projectId" as "project_id",
            p.name as "project_name",
            di.sha as "sha",
            dt.sub_type as "sub_type",
            d."deletedAt"::timestamptz as "deleted_at"
        FROM
            "Document" d
        LEFT JOIN document_sub_type dt ON dt.document_id = d.id
        LEFT JOIN LATERAL (
            SELECT
                i.id,
                i.sha,
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
                i."createdAt" DESC
            LIMIT 1
        ) di ON true
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
                p.name
            FROM "Project" p
            WHERE p.id = d."projectId"
        ) p ON d."projectId" IS NOT NULL
        WHERE
            d.id = $1
        LIMIT 1;


-- name: GetDocumentVersion :one
SELECT
            d.id as "document_id",
            d.owner as "owner",
            d.name as "document_name",
            COALESCE(di.id, db.id) as "document_version_id",
            d."branchedFromId" as "branched_from_id",
            d."branchedFromVersionId" as "branched_from_version_id",
            d."documentFamilyId" as "document_family_id",
            d."createdAt"::timestamptz as "created_at",
            d."updatedAt"::timestamptz as "updated_at",
            d."fileType" as "file_type",
            db.bom_parts as "document_bom",
            di.modification_data as "modification_data",
            d."projectId" as "project_id",
            p.name as "project_name",
            di.sha as "sha",
            dt.sub_type as "sub_type",
            d."deletedAt"::timestamptz as deleted_at
        FROM
            "Document" d
        LEFT JOIN document_sub_type dt ON dt.document_id = d.id
        LEFT JOIN LATERAL (
            SELECT
                i.id,
                i.sha,
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
            AND
                i.id = $2
        ) di ON true
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
            AND
                b.id = $2
        ) db ON d."fileType" = 'docx'
        LEFT JOIN LATERAL (
            SELECT
                p.name
            FROM "Project" p
            WHERE p.id = d."projectId"
        ) p ON d."projectId" IS NOT NULL
        WHERE
            d.id = $1
        LIMIT 1;


-- name: GetDocumentBom :one
SELECT b.id as "id"
        FROM "DocumentBom" b
        WHERE b."documentId" = $1
        ORDER BY b."createdAt" DESC
        LIMIT 1;


-- name: GetDocumentBom2 :many
SELECT
            bp.id as id,
            bp.sha as sha,
            bp.path as path
        FROM
            "BomPart" bp
        WHERE
            bp."documentBomId" = $1;


-- name: GetBomParts :many
SELECT
            bp.sha as sha,
            bp.path as path,
            bp.id as id
        FROM "BomPart" bp
        JOIN "DocumentBom" db ON bp."documentBomId" = db.id
        WHERE db."documentId" = $1;


-- name: GetBomPartsBulkTsx :many
SELECT 
        bp.sha as sha,
        bp.path as path,
        bp.id as id
        FROM "BomPart" bp
        JOIN "DocumentBom" db ON bp."documentBomId" = db.id
        WHERE db."documentId" = ANY($1::text[]);


-- name: GetDocumentSha :one
SELECT
            di.sha
        FROM
            "DocumentInstance" di
        WHERE di."documentId" = $1 AND di.id = $2
        LIMIT 1;

