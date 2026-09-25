-- name: CreateBlankDocx :one
SELECT name FROM "Project" WHERE id = $1;


-- name: CreateBlankDocx2 :one
INSERT INTO "Document" (owner, name, "fileType", "projectId")
            VALUES ($1, $2, $3, $4)
            RETURNING id;


-- name: CreateBlankDocx3 :one
INSERT INTO "DocumentFamily" ("rootDocumentId")
                VALUES ($1)
                RETURNING id;


-- name: CreateBlankDocx4 :exec
UPDATE "Document" SET "documentFamilyId" = $1 WHERE id = $2;


-- name: CreateBlankDocx5 :one
INSERT INTO "DocumentBom" ("documentId")
                VALUES ($1)
                RETURNING id, "createdAt"::timestamptz as created_at, "updatedAt"::timestamptz as updated_at;

