-- name: CreateDocumentEmailRecord :exec
INSERT INTO "document_email" (document_id, email_attachment_id)
            VALUES ($1, $2);

