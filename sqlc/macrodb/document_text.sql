-- name: GetDocumentText :one
SELECT content FROM "DocumentText" WHERE "documentId" = $1;

