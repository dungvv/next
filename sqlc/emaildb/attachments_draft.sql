-- name: InsertDraftAttachment :exec
INSERT INTO email_attachments_drafts (
                id, draft_id, file_name, content_type, sha, size, s3_key
            )
            -- if the message belongs to a different link_id, nothing will be returned from this
            -- and thus nothing will be inserted
                SELECT $1, $2, $3, $4, $5, $6, $7
                FROM email_messages m
                WHERE m.id = $2 AND m.link_id = $8;


-- name: GetTotalAttachmentsSizeByDraftId :one
SELECT SUM(ead.size)::BIGINT
                FROM email_attachments_drafts ead
                JOIN email_messages m ON ead.draft_id = m.id
                WHERE ead.draft_id = $1 AND m.link_id = $2;


-- name: DeleteDraftAttachment :exec
DELETE FROM email_attachments_drafts ead
                USING email_messages m
                WHERE ead.draft_id = m.id
                AND ead.id = $1 AND ead.draft_id = $2 AND m.link_id = $3;


-- name: FetchDraftAttachmentsByDraftId :many
SELECT ead.id, ead.draft_id, ead.file_name, ead.content_type, ead.sha, ead.size, ead.s3_key
            FROM email_attachments_drafts ead
            JOIN email_messages m ON ead.draft_id = m.id
            WHERE ead.draft_id = $1 AND m.link_id = $2
            ORDER BY ead.file_name ASC;


-- name: FetchDbDraftAttachmentsInBulk :many
SELECT id, draft_id, file_name, content_type, sha, size, s3_key
            FROM email_attachments_drafts
            WHERE "draft_id" = ANY($1::uuid[])
            ORDER BY draft_id, file_name ASC;

