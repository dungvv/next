-- name: ListDocumentsWithAccess :many
WITH user_source_ids AS (
            SELECT cp.channel_id::text as source_id FROM comms_channel_participants cp
                WHERE cp.user_id = $1 AND cp.left_at IS NULL
            UNION ALL
            SELECT t.team_id::text FROM team_user t
                WHERE t.user_id = $1
            UNION ALL
            SELECT $1
        ),
        user_accessible_documents AS (
            SELECT DISTINCT ON (entity_id) entity_id, access_level
            FROM entity_access
            WHERE source_id = ANY(SELECT source_id FROM user_source_ids)
              AND entity_type = 'document'
            ORDER BY entity_id,
                CASE access_level::text
                    WHEN 'owner' THEN 4
                    WHEN 'edit' THEN 3
                    WHEN 'comment' THEN 2
                    WHEN 'view' THEN 1
                    ELSE 0
                END DESC
        )
        SELECT
            d.id as "document_id",
            d.name as "document_name",
            d.owner as "owner",
            d."fileType" as "file_type",
            d."projectId" as "project_id",
            d."createdAt"::timestamptz as "created_at",
            d."updatedAt"::timestamptz as "updated_at",
            d."deletedAt"::timestamptz as "deleted_at",
            uad.access_level::text as "access_level"
        FROM "Document" d
        INNER JOIN user_accessible_documents uad ON uad.entity_id = d.id::uuid
        WHERE d."deletedAt" IS NULL
        AND ($2::text[] IS NULL OR d."fileType" = ANY($2::text[]))
        AND (
            CASE uad.access_level::text
                WHEN 'owner' THEN 4
                WHEN 'edit' THEN 3
                WHEN 'comment' THEN 2
                WHEN 'view' THEN 1
                ELSE 0
            END >=
            CASE $3::text
                WHEN 'owner' THEN 4
                WHEN 'edit' THEN 3
                WHEN 'comment' THEN 2
                WHEN 'view' THEN 1
                ELSE 0
            END
        )
        ORDER BY d."updatedAt" DESC
        LIMIT $4 OFFSET $5;

