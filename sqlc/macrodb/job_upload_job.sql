-- name: UpdateUploadJob :exec
UPDATE "UploadJob" SET "documentId" = $1 WHERE "jobId" = $2;

