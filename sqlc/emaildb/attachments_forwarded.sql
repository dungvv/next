-- name: InsertForwardedAttachment :exec
INSERT INTO email_attachments_fwd (message_id, attachment_id)
            SELECT $1, $2
            FROM email_messages m
            WHERE m.id = $1 AND m.link_id = $3
        ON CONFLICT DO NOTHING;


-- name: DeleteForwardedAttachment :exec
DELETE FROM email_attachments_fwd eaf
        USING email_messages m
        WHERE eaf.message_id = m.id
        AND eaf.message_id = $1 AND eaf.attachment_id = $2 AND m.link_id = $3;


-- name: FetchForwardedAttachmentsByDraftId :many
SELECT
            eaf.attachment_id,
            eaf.message_id AS draft_id,
            ea.provider_attachment_id,
            orig_msg.provider_id AS "message_provider_id",
            ea.filename,
            ea.mime_type,
            ea.size_bytes
        FROM email_attachments_fwd eaf
        JOIN email_messages draft_msg ON eaf.message_id = draft_msg.id
        JOIN email_attachments ea ON eaf.attachment_id = ea.id
        JOIN email_messages orig_msg ON ea.message_id = orig_msg.id
        WHERE eaf.message_id = $1 AND draft_msg.link_id = $2
        ORDER BY ea.filename ASC;


-- name: FetchForwardedAttachmentsInBulk :many
SELECT
            eaf.attachment_id,
            eaf.message_id AS draft_id,
            ea.provider_attachment_id,
            orig_msg.provider_id AS "message_provider_id",
            ea.filename,
            ea.mime_type,
            ea.size_bytes
        FROM email_attachments_fwd eaf
        JOIN email_attachments ea ON eaf.attachment_id = ea.id
        JOIN email_messages orig_msg ON ea.message_id = orig_msg.id
        WHERE eaf."message_id" = ANY($1::uuid[])
        ORDER BY eaf.message_id, ea.filename ASC;

