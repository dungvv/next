-- name: DeleteDocumentThread :one
UPDATE "Thread"
            SET "deletedAt" = NOW()
            WHERE id = $1 and "deletedAt" IS NULL
            RETURNING "deletedAt";


-- name: DeleteDocumentThread2 :exec
UPDATE "Comment"
            SET "deletedAt" = NOW()
            WHERE "threadId" = $1;

