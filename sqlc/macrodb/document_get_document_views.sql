-- name: GetDocumentViews :many
SELECT
                u.email
            FROM "UserHistory" uh
            JOIN "User" u ON uh."userId" = u.id
            WHERE uh."itemId" = $1 AND uh."itemType" = 'document';


-- name: GetDocumentViewCount :one
SELECT COUNT(*)
            FROM "DocumentView" dv
            WHERE dv."document_id" = $1;

