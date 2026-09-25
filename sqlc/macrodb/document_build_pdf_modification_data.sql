-- name: GetCompletePdfModificationData :many
SELECT 
            t.id as thread_id, 
            t.resolved as is_resolved,
            a.uuid as anchor_uuid, 
            a.page, 
            a."originalPage" as original_page,
            a."originalIndex" as original_index,
            a."shouldLockOnSave" as should_lock_on_save,
            a."wasEdited" as was_edited,
            a."wasDeleted" as was_deleted,
            a."allowableEdits" as allowable_edits,
            a."xPct" as x_pct,
            a."yPct" as y_pct,
            a."widthPct" as width_pct,
            a."heightPct" as height_pct,
            a.rotation
        FROM "Thread" t
        JOIN "PdfPlaceableCommentAnchor" a ON t."id" = a."threadId"
        WHERE t."documentId" = $1;


-- name: GetCompletePdfModificationData2 :many
SELECT 
                id, 
                owner, 
                text as content, 
                "createdAt" as created_at, 
                "updatedAt" as updated_at,
                "order"
            FROM "Comment" 
            WHERE "threadId" = $1
            ORDER BY "order";


-- name: GetCompletePdfModificationData3 :many
SELECT 
            a.uuid, 
            a."threadId" as thread_id,
            a.page, 
            a.red, 
            a.green, 
            a.blue, 
            a.alpha,
            a.type as highlight_type, 
            a.text,
            a."pageViewportWidth" as page_viewport_width, 
            a."pageViewportHeight" as page_viewport_height, 
            a."createdAt"::timestamptz as created_at, 
            a."updatedAt"::timestamptz as updated_at
        FROM "PdfHighlightAnchor" a
        WHERE a."documentId" = $1;


-- name: GetCompletePdfModificationData4 :many
SELECT 
                phr.top,
                phr.left,
                phr.width,
                phr.height
            FROM "PdfHighlightRect" phr 
            WHERE phr."pdfHighlightAnchorId" = $1;


-- name: GetCompletePdfModificationData5 :one
SELECT resolved as is_resolved
                FROM "Thread"
                WHERE id = $1;


-- name: GetCompletePdfModificationData6 :many
SELECT 
                    id, 
                    owner, 
                    text as content, 
                    "createdAt" as created_at, 
                    "updatedAt" as updated_at,
                    "order"
                FROM "Comment" 
                WHERE "threadId" = $1
                ORDER BY "order";

