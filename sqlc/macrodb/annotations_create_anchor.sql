-- name: ValidateThreadExists :one
SELECT 1
    FROM "Thread" t
    WHERE t.id = $1 AND t."deletedAt" IS NULL;


-- name: AttachAnchorToThread :exec
INSERT INTO "ThreadAnchor" ("threadId", "anchorId", "anchorTableName")
    VALUES ($1, $2, $3::"anchor_table_name");


-- name: CreatePdfHighlightAnchor :exec
INSERT INTO "PdfHighlightRect" ("pdfHighlightAnchorId", "top", "left", "width", "height")
    SELECT unnest($1::uuid[]), unnest($2::double precision[]), unnest($3::double precision[]), unnest($4::double precision[]), unnest($5::double precision[]);

