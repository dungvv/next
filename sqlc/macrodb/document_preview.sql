-- name: BatchGetDocumentPreviewV22 :many
SELECT
                d.id as "document_id",
                d.name as "document_name",
                d."fileType" as file_type,
                d.owner as "owner",
                d."updatedAt"::timestamptz as "updated_at",
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
            WHERE
                d."id" = ANY($1::text[]);

