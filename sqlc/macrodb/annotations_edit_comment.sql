-- name: EditDocumentComment :one
SELECT c.owner, t."documentId" as document_id, d.name as document_name, d."fileType" as file_type, dt.sub_type as "sub_type", d.owner as document_owner
        FROM "Comment" c
        JOIN "Thread" t ON c."threadId" = t.id
        JOIN "Document" d ON t."documentId" = d.id
        LEFT JOIN document_sub_type dt ON dt.document_id = d.id
        WHERE c.id = $1 and c."deletedAt" IS NULL AND t."deletedAt" IS NULL;


-- name: EditDocumentComment2 :one
UPDATE "Comment" c
        SET "text" = $1, "metadata" = $2, "updatedAt" = NOW()
        WHERE "id" = $3 AND "deletedAt" IS NULL
        RETURNING 
            c.id as comment_id, 
            c."threadId" as thread_id, 
            c.owner, 
            c.sender, 
            c.text, 
            c.metadata, 
            c."createdAt"::timestamptz as created_at, 
            c."updatedAt"::timestamptz as updated_at, 
            c."deletedAt"::timestamptz as deleted_at, 
            c.order;

