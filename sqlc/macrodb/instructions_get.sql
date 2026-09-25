-- name: GetInstructionsDocument :one
SELECT "documentId" as "document_id"
            FROM "InstructionsDocuments" id
            JOIN "Document" d ON d."id" = id."documentId"
            WHERE "userId" = $1 AND d."deletedAt" IS NULL;

