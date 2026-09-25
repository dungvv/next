-- name: GetPartById :one
SELECT reference, "documentId"
            FROM "DocumentTextParts"
            WHERE id = $1;

