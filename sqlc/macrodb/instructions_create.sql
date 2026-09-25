-- name: CreateInstructionsDocument :exec
DELETE FROM "InstructionsDocuments" WHERE "userId" = $1;


-- name: InsertInstructionsDocumentOn :exec
INSERT INTO "InstructionsDocuments" ("documentId", "userId")
            VALUES ($1, $2);

