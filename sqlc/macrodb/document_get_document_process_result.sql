-- name: GetDocumentProcessContentFromJobId :one
SELECT
            d."content"
        FROM "DocumentProcessResult" d
        JOIN "JobToDocumentProcessResult" j ON d.id = j."documentProcessResultId"
        WHERE j."jobId" = $1 AND d."documentId" = $2;


-- name: GetDocumentProcessContent :one
SELECT
            "content"
        FROM "DocumentProcessResult"
        WHERE "documentId" = $1 AND "jobType" = $2;

