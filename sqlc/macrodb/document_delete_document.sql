-- name: DeleteDocument :one
SELECT "sharePermissionId" as share_permission_id
            FROM "DocumentPermission"
            WHERE "documentId"=$1;


-- name: DeleteDocument2 :exec
DELETE FROM "Document" WHERE id = $1;


-- name: DeleteDocumentVersion :one
SELECT
            (SELECT COUNT(*) FROM "DocumentInstance" WHERE "DocumentInstance"."documentId" = $1) +
            (SELECT COUNT(*) FROM "DocumentBom" WHERE "DocumentBom"."documentId" = $1) AS total_count;


-- name: DeleteDocumentVersion2 :exec
DELETE FROM "DocumentBom" WHERE id = $2 and "documentId" = $1;


-- name: DeleteDocumentVersion3 :exec
DELETE FROM "DocumentInstance" WHERE id = $2 and "documentId" = $1;


-- name: GetShasForDeletion :many
SELECT bp.sha
        FROM "BomPart" bp
        JOIN "DocumentBom" db ON bp."documentBomId" = db.id
        WHERE db."documentId" = $1;

