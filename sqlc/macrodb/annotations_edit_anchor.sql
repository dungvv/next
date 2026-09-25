-- name: EditPdfFreeCommentAnchor :one
SELECT a.owner, d.owner as document_owner
        FROM "PdfPlaceableCommentAnchor" a
        JOIN "Thread" t ON a."threadId" = t.id
        JOIN "Document" d ON a."documentId" = d.id
        WHERE a.uuid = $1 AND t."deletedAt" IS NULL;


-- name: EditPdfFreeCommentAnchor2 :one
UPDATE "PdfPlaceableCommentAnchor" SET
            "page" = COALESCE($1, "page"),
            "originalPage" = COALESCE($2, "originalPage"),
            "originalIndex" = COALESCE($3, "originalIndex"),
            "xPct" = COALESCE($4, "xPct"),
            "yPct" = COALESCE($5, "yPct"),
            "widthPct" = COALESCE($6, "widthPct"),
            "heightPct" = COALESCE($7, "heightPct"),
            "rotation" = COALESCE($8, "rotation"),
            "allowableEdits" = COALESCE($9, "allowableEdits"),
            "wasEdited" = COALESCE($10, "wasEdited"),
            "wasDeleted" = COALESCE($11, "wasDeleted"),
            "shouldLockOnSave" = COALESCE($12, "shouldLockOnSave")
        WHERE uuid = $13
        RETURNING 
            uuid, 
            "documentId" as document_id,
            owner, 
            "threadId" as thread_id,
            root_id,
            page, 
            "originalPage" as original_page, 
            "originalIndex" as original_index, 
            "xPct" as x_pct, 
            "yPct" as y_pct, 
            "widthPct" as width_pct, 
            "heightPct" as height_pct, 
            rotation,
            "allowableEdits" as allowable_edits, 
            "wasEdited" as was_edited, 
            "wasDeleted" as was_deleted, 
            "shouldLockOnSave" as should_lock_on_save;

