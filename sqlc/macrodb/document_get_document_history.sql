-- name: GetDocumentHistoryInfo :many
SELECT
            c."id" as "item_id",
            c."owner" as "owner",
            c."fileType" as "file_type",
            c."name" as "file_name",
            c."createdAt" as "created_at",
            c."updatedAt" as "updated_at",
            c."deletedAt" as "deleted_at",
            uh."updatedAt" as "viewed_at",
            c."projectId" as "project_id",
            dt.sub_type as "sub_type"
        FROM
            "Document" c
        LEFT JOIN document_sub_type dt ON dt.document_id = c.id
        LEFT JOIN
            "UserHistory" uh ON uh."itemId" = c."id"
                AND uh."userId" = $1
                AND uh."itemType" = 'document'
        WHERE
            c."id" = ANY($2::text[])
        ORDER BY
            c."updatedAt" DESC;

