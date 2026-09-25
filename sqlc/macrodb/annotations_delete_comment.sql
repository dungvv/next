-- name: DeleteDocumentComment :one
SELECT c.owner, t.id as thread_id, d.owner as document_owner, d.id as document_id, ta."anchorId" as "anchor_id", ta."anchorTableName" as "anchor_table_name"
        FROM "Comment" c
        JOIN "Thread" t ON c."threadId" = t.id
        JOIN "Document" d ON t."documentId" = d.id
        LEFT JOIN "ThreadAnchor" ta ON ta."threadId" = t.id
        WHERE c.id = $1 AND c."deletedAt" IS NULL;


-- name: DeleteDocumentComment2 :one
SELECT c.id as comment_id
        FROM "Comment" c
        JOIN "Thread" t ON c."threadId" = t.id
        WHERE t.id = $1 AND c."deletedAt" IS NULL
        ORDER BY c."order", c."createdAt" ASC
        LIMIT 1;


-- name: DeleteDocumentComment3 :exec
UPDATE "Comment"
        SET "deletedAt" = NOW()
        WHERE "id" = $1 AND "deletedAt" IS NULL;

