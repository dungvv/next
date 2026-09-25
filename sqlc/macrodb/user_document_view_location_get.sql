-- name: GetUserDocumentViewLocation :one
SELECT user_id, document_id, location
        FROM "UserDocumentViewLocation"
        WHERE user_id = $1 AND document_id = $2;

