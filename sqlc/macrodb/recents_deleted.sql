-- name: GetRecentlyDeleted :many
SELECT
            'document' as "item_type",
            d.id as "id",
            CAST(COALESCE(di.id, db.id) as TEXT) as "document_version_id",
            d.owner as "user_id",
            d.name as "name",
            d."fileType" as "file_type",
            d."createdAt"::timestamptz as "created_at",
            d."updatedAt"::timestamptz as "updated_at",
            d."deletedAt"::timestamptz as "deleted_at",
            d."projectId" as "project_id",
            NULL::bool as "is_persistent",
            dt.sub_type as "sub_type",
            CASE 
                WHEN dt.sub_type = 'task' 
                    AND ep_status.values->'value' ? $2
                THEN true 
                WHEN dt.sub_type = 'task'
                THEN false
                ELSE NULL 
            END::bool as "is_completed"
        FROM "Document" d
        LEFT JOIN document_sub_type dt ON dt.document_id = d.id
        LEFT JOIN entity_properties ep_status 
            ON dt.sub_type = 'task'
            AND ep_status.entity_id = d.id 
            AND ep_status.entity_type = 'TASK'
            AND ep_status.property_definition_id = $3
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
        ) db ON true
        LEFT JOIN LATERAL (
            SELECT
                i.id
            FROM
                "DocumentInstance" i
            WHERE
                i."documentId" = d.id
            ORDER BY
                i."updatedAt" DESC
            LIMIT 1
        ) di ON true
        WHERE d.owner = $1 AND d."deletedAt" IS NOT NULL
        UNION ALL
        SELECT
            'chat' as "item_type",
            c.id as "id",
            NULL::int8 as "document_version_id",
            c."userId" as "user_id",
            c.name as "name",
            NULL::text as "file_type",
            c."createdAt"::timestamptz as "created_at",
            c."updatedAt"::timestamptz as "updated_at",
            c."deletedAt"::timestamptz as "deleted_at",
            c."projectId" as "project_id",
            c."isPersistent" as "is_persistent",
            NULL::document_sub_type_value as "sub_type",
            NULL::bool as "is_completed"
        FROM "Chat" c
        WHERE c."userId" = $1 AND c."deletedAt" IS NOT NULL
        UNION ALL
        SELECT
            'project' as "item_type",
            p.id as "id",
            NULL::int8 as "document_version_id",
            p."userId" as "user_id",
            p.name as "name",
            NULL::text as "file_type",
            p."createdAt"::timestamptz as "created_at",
            p."updatedAt"::timestamptz as "updated_at",
            p."deletedAt"::timestamptz as "deleted_at",
            p."parentId" as "project_id",
            NULL::bool as "is_persistent",
            NULL::document_sub_type_value as "sub_type",
            NULL::bool as "is_completed"
        FROM "Project" p
        WHERE p."userId" = $1 AND p."deletedAt" IS NOT NULL
    ORDER BY deleted_at DESC;

