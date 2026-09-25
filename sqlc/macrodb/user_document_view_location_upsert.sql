-- name: UpsertUserDocumentViewLocation :exec
INSERT INTO "UserDocumentViewLocation" (user_id, document_id, location)
        VALUES ($1, $2, $3)
        ON CONFLICT (user_id, document_id) 
        DO UPDATE SET location = EXCLUDED.location, updated_at = NOW();

