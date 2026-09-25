-- name: CreateDocumentTxn :exec
INSERT INTO document_sub_type (document_id, sub_type) 
            VALUES ($1, $2);


-- name: CreateDocumentTxn2 :one
INSERT INTO "DocumentBom" ("documentId", "createdAt", "updatedAt")
                VALUES ($1, $2, $2)
                RETURNING id, "createdAt"::timestamptz as created_at, "updatedAt"::timestamptz as updated_at;


-- name: CreateDocumentTxn3 :one
INSERT INTO "DocumentInstance" ("documentId", "sha", "createdAt", "updatedAt")
                    VALUES ($1, $2, $3, $3)
                    RETURNING id, sha, "createdAt"::timestamptz as created_at, "updatedAt"::timestamptz as updated_at;


-- name: InsertDocumentNoId :one
INSERT INTO "Document" (owner, name, "fileType", "projectId", "createdAt", "updatedAt") 
            VALUES ($1, $2, $3, $4, $5, $5)
            RETURNING id;


-- name: InsertDocumentWithId :exec
INSERT INTO "Document" (id, owner, name, "fileType", "projectId", "createdAt", "updatedAt")
            VALUES ($1, $2, $3, $4, $5, $6, $6);

