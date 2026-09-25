-- name: GetCommentThread :one
SELECT 
            t.id as thread_id, 
            t.resolved, 
            t."documentId" as document_id, 
            t."createdAt"::timestamptz as created_at, 
            t."updatedAt"::timestamptz as updated_at, 
            t."deletedAt"::timestamptz as deleted_at, 
            t.metadata, 
            t.owner
        FROM "Thread" t
        WHERE t."id" = $1
            AND t."deletedAt" IS NULL;


-- name: GetCommentThread2 :many
SELECT 
            c.id as comment_id, 
            c."threadId" as thread_id, 
            c.owner, 
            c.sender, 
            c.text, 
            c.metadata, 
            c."createdAt"::timestamptz as created_at, 
            c."updatedAt"::timestamptz as updated_at, 
            c."deletedAt"::timestamptz as deleted_at, 
            c.order
        FROM "Comment" c
        JOIN "Thread" t ON c."threadId" = t.id
        WHERE c."threadId" = $1
            AND c."deletedAt" IS NULL
            AND t."deletedAt" IS NULL
        ORDER BY c."createdAt" ASC;


-- name: GetDocumentComments :many
SELECT 
            t.id as thread_id, 
            t.resolved, 
            t."documentId" as document_id, 
            t."createdAt"::timestamptz as created_at, 
            t."updatedAt"::timestamptz as updated_at, 
            t."deletedAt"::timestamptz as deleted_at, 
            t.metadata, 
            t.owner
        FROM "Thread" t
        WHERE t."documentId" = $1 AND t."deletedAt" IS NULL;


-- name: GetDocumentComments2 :many
SELECT 
            c.id as comment_id, 
            c."threadId" as thread_id, 
            c.owner, 
            c.sender, 
            c.text, 
            c.metadata, 
            c."createdAt"::timestamptz as created_at, 
            c."updatedAt"::timestamptz as updated_at, 
            c."deletedAt"::timestamptz as deleted_at, 
            c.order
        FROM "Comment" c
        JOIN "Thread" t ON c."threadId" = t.id
        WHERE t."documentId" = $1 AND t."deletedAt" IS NULL AND c."deletedAt" IS NULL
        ORDER BY c."createdAt" ASC;


-- name: FetchPdfPlaceableAnchors :many
SELECT 
            pa.uuid, 
            pa."documentId" as document_id, 
            pa.owner, 
            pa."threadId" as thread_id,
            pa.root_id,
            pa.page, 
            pa."originalPage" as original_page, 
            pa."originalIndex" as original_index, 
            pa."xPct" as x_pct, 
            pa."yPct" as y_pct, 
            pa."widthPct" as width_pct, 
            pa."heightPct" as height_pct,
            pa.rotation, 
            pa."allowableEdits" as allowable_edits,
            pa."wasEdited" as was_edited,
            pa."wasDeleted" as was_deleted,
            pa."shouldLockOnSave" as should_lock_on_save
        FROM "PdfPlaceableCommentAnchor" pa
        LEFT JOIN "Thread" t ON pa."threadId" = t.id
        WHERE pa."documentId" = $1
        AND t."deletedAt" IS NULL;


-- name: FetchPdfHighlightAnchors :many
SELECT 
            ph.uuid, 
            ph."documentId" as document_id,
            ph.owner, 
            ph."threadId" as thread_id,
            ph.root_id,
            ph.page, 
            ph.red,
            ph.green, 
            ph.blue, 
            ph.alpha, 
            ph.type as highlight_type, 
            ph.text, 
            ph."pageViewportWidth" as page_viewport_width, 
            ph."pageViewportHeight" as page_viewport_height, 
            ph."createdAt"::timestamptz as created_at, 
            ph."updatedAt"::timestamptz as updated_at, 
            ph."deletedAt"::timestamptz as deleted_at, 
            array_agg((phr.id, phr.top, phr.left, phr.width, phr.height)) as "highlight_rects"
        FROM "PdfHighlightAnchor" ph
        JOIN "PdfHighlightRect" phr ON ph.uuid = phr."pdfHighlightAnchorId"
        LEFT JOIN "Thread" t ON ph."threadId" = t.id
        WHERE ph."documentId" = $1
        AND ph."deletedAt" IS NULL
        AND t."deletedAt" IS NULL
        GROUP BY ph.uuid, ph.owner, ph."threadId", ph.root_id, ph.page, ph.red, ph.green, ph.blue, ph.alpha, ph.type, ph.text, ph."pageViewportWidth", ph."pageViewportHeight", ph."createdAt", ph."updatedAt", ph."deletedAt";

