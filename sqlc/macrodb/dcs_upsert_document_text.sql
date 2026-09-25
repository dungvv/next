-- name: UpsertDocumentText :exec
INSERT INTO "DocumentText" ("documentId", "content", "tokenCount")
        VALUES ($1, $2, $3)
        ON CONFLICT ("documentId")
        DO UPDATE SET "content" = EXCLUDED."content", "tokenCount" = EXCLUDED."tokenCount";

