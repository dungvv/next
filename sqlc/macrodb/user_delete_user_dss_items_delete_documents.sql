-- name: DeleteUserDocuments :many
SELECT id FROM "Document" WHERE "owner" = $1;


-- name: DeleteUserDocuments2 :exec
DELETE FROM "SharePermission" sp
        USING "DocumentPermission" dp 
        WHERE dp."sharePermissionId" = sp.id
        AND dp."documentId" = ANY($1::text[]);


-- name: DeleteUserDocuments3 :exec
DELETE FROM "Document" 
        WHERE "id" = ANY($1::text[]);

