-- name: DeleteUserDocumentViewLocation :exec
DELETE FROM "UserDocumentViewLocation"
        WHERE user_id = $1 AND document_id = $2;

