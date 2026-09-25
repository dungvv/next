-- name: RevertDeleteDocument :one
UPDATE "Document"
        SET "deletedAt" = NULL
        WHERE id = $1
        RETURNING owner as owner;


-- name: RevertDeleteDocument2 :exec
UPDATE "Document" SET "projectId" = NULL WHERE "id" = $1;

