-- name: InsertAttachmentSfs :exec
INSERT INTO email_attachments_sfs (id, attachment_id, sfs_id)
        SELECT $1, $2, $3
        WHERE $2::uuid IS NULL
           OR EXISTS (SELECT 1 FROM email_attachments WHERE id = $2);


-- name: DeleteAttachmentSfs :exec
DELETE FROM email_attachments_sfs
        WHERE id = $1;

