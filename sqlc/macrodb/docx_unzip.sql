-- name: UpdateUploadedStatus :exec
UPDATE "Document"
        SET "uploaded" = true,
            "contentState" = CASE
                WHEN "contentState" = 'ready' AND "contentLocation" = 'converted_pdf'
                    THEN 'ready'
                ELSE 'pending'
            END,
            "contentLocation" = 'converted_pdf'
        WHERE id = $1;


-- name: GetJobForDocxUpload :one
SELECT "jobId" as job_id, "jobType" as job_type FROM "UploadJob" WHERE "documentId" = $1;

