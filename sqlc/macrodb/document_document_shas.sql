-- name: GetDocumentShas :many
SELECT 
        bp.sha
    FROM "BomPart" bp
    WHERE bp."documentBomId" = $1;


-- name: GetDocumentShasByDocumentId :many
SELECT 
        bp.sha
    FROM "BomPart" bp
    JOIN "DocumentBom" db ON bp."documentBomId" = db.id
    WHERE db."documentId" = $1
    AND db.id = (
        SELECT db_inner.id
        FROM "DocumentBom" db_inner
        WHERE db_inner."documentId" = $1
        ORDER BY db_inner."updatedAt" DESC
        LIMIT 1
    );

