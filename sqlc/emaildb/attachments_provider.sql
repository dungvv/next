-- name: InsertAttachments :many
SELECT ea.id, ea.message_id, ea.provider_attachment_id, ea.filename, ea.mime_type, ea.size_bytes, ea.content_id, eas.sfs_id as "sfs_id", ea.created_at
        FROM email_attachments ea LEFT JOIN email_attachments_sfs eas on ea.id = eas.attachment_id
        WHERE message_id = $1;


-- name: InsertAttachments2 :exec
DELETE FROM email_attachments
            WHERE "id" = ANY($1::uuid[]);


-- name: InsertAttachments3 :exec
WITH input_rows (
            id, message_id, provider_attachment_id, filename, mime_type, size_bytes, content_id
        ) AS (
           SELECT unnest($1::uuid[]), unnest($2::uuid[]), unnest($3::text[]), unnest($4::varchar[]), unnest($5::varchar[]), unnest($6::bigint[]), unnest($7::varchar[])
        )
        INSERT INTO email_attachments (
            id, message_id, provider_attachment_id, filename, mime_type, size_bytes, content_id
        )
        SELECT id, message_id, provider_attachment_id, filename, mime_type, size_bytes, content_id
        FROM input_rows
        ON CONFLICT (message_id, provider_attachment_id)
        DO NOTHING;


-- name: SyncThreadCalendarFlag :one
SELECT thread_id FROM email_messages WHERE id = $1;


-- name: FetchDbAttachmentsInBulk :many
SELECT ea.id, ea.message_id, ea.provider_attachment_id, ea.filename, ea.mime_type, ea.size_bytes, ea.content_id, eas.sfs_id as "sfs_id", ea.created_at
        FROM email_attachments ea
        LEFT JOIN email_attachments_sfs eas ON ea.id = eas.attachment_id
        WHERE ea."message_id" = ANY($1::uuid[])
        ORDER BY ea.message_id, ea.filename NULLS LAST;


-- name: DeleteMessageAttachments :exec
DELETE FROM email_attachments WHERE message_id = $1;


-- name: FetchAttachmentById :one
SELECT 
            a.id, a.message_id, a.provider_attachment_id, 
            a.filename, a.mime_type, a.size_bytes, 
            a.content_id, eas.sfs_id as "sfs_id", a.created_at, m.provider_id as "message_provider_id"
        FROM email_attachments a
        JOIN email_messages m ON a.message_id = m.id
        LEFT JOIN email_attachments_sfs eas ON a.id = eas.attachment_id
        WHERE a.id = $1 AND m.link_id = $2;


-- name: GetAttachmentsByThreadIds :many
SELECT 
            a.id,
            a.message_id,
            a.provider_attachment_id,
            a.filename,
            a.mime_type,
            a.size_bytes,
            a.content_id,
            eas.sfs_id as "sfs_id",
            a.created_at,
            m.thread_id
        FROM 
            email_attachments a
        JOIN
            email_messages m ON a.message_id = m.id
        LEFT JOIN
            email_attachments_sfs eas ON a.id = eas.attachment_id
        WHERE
            m."thread_id" = ANY($1::uuid[])
        ORDER BY 
            a.created_at ASC;


-- name: GetDocumentIdByAttIdAndLink :one
SELECT document_id
        FROM document_email de
        INNER JOIN "Document" d on de.document_id = d.id
        INNER JOIN email_attachments ea on de.email_attachment_id = ea.id
        INNER JOIN email_messages em on ea.message_id = em.id
        WHERE em.link_id = $1 AND email_attachment_id = $2 AND d."deletedAt" IS NULL;


-- name: GetDocumentIdByAttId :one
SELECT document_id
        FROM document_email de
        INNER JOIN "Document" d on de.document_id = d.id
        INNER JOIN email_attachments ea on de.email_attachment_id = ea.id
        INNER JOIN email_messages em on ea.message_id = em.id
        WHERE email_attachment_id = $1 AND d."deletedAt" IS NULL;


-- name: GetThreadIdForAttachment :one
SELECT et.id as thread_id, et.link_id
        FROM email_attachments ea
        INNER JOIN email_messages em ON ea.message_id = em.id
        INNER JOIN email_threads et ON em.thread_id = et.id
        WHERE ea.id = $1;

