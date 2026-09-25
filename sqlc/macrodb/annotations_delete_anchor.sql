-- name: DeletePdfHighlightAnchor :one
SELECT a.owner, a."threadId" as thread_id, d.owner as document_owner, d.id as document_id
        FROM "PdfHighlightAnchor" a
        JOIN "Document" d ON a."documentId" = d.id
        WHERE a.uuid = $1 AND a."deletedAt" IS NULL;


-- name: DeletePdfHighlightAnchor2 :exec
UPDATE "PdfHighlightAnchor"
        SET "deletedAt" = NOW()
        WHERE uuid = $1 AND "deletedAt" IS NULL;


-- name: DeletePdfHighlightAnchor3 :exec
UPDATE "PdfHighlightAnchor"
            SET "threadId" = NULL
            WHERE uuid = $1 AND "deletedAt" IS NULL;

