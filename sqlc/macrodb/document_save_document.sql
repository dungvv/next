-- name: InsertAllCommentData :many
SELECT c.id, c."createdAt" as created_at, c."updatedAt" as updated_at
        FROM "Comment" c
        JOIN "Thread" t ON c."threadId" = t.id
        WHERE t."documentId" = $1;


-- name: InsertAllCommentData2 :many
SELECT a.uuid, a."createdAt" as created_at, a."updatedAt" as updated_at
        FROM "PdfHighlightAnchor" a
        WHERE a."documentId" = $1;


-- name: InsertAllCommentData3 :exec
DELETE FROM "Thread" WHERE "documentId" = $1;


-- name: InsertAllCommentData4 :exec
DELETE FROM "PdfHighlightAnchor" WHERE "documentId" = $1;


-- name: InsertAllCommentData5 :one
INSERT INTO "Thread" ("owner", "documentId", "createdAt", "updatedAt", "resolved")
            VALUES ($1, $2, $3, $4, $5)
            RETURNING id;


-- name: InsertAllCommentData6 :one
INSERT INTO "PdfPlaceableCommentAnchor" ("uuid", "documentId", "owner", "page", "originalPage", "originalIndex", "shouldLockOnSave", "xPct", "yPct", "widthPct", "heightPct", "rotation", "threadId", "wasEdited", "wasDeleted", "allowableEdits")
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
            RETURNING uuid;


-- name: InsertAllCommentData7 :one
INSERT INTO "Comment" ("threadId", "owner", "sender", "text", "createdAt", "updatedAt", "order")
                VALUES ($1, $2, $3, $4, $5, $6, $7)
                RETURNING id;


-- name: InsertAllCommentData8 :one
INSERT INTO "Thread" ("owner", "documentId", "createdAt", "updatedAt", "resolved")
                            VALUES ($1, $2, $3, $4, $5)
                            RETURNING id;


-- name: InsertAllCommentData9 :one
INSERT INTO "Comment" ("threadId", "owner", "sender", "text", "createdAt", "updatedAt", "order")
                                VALUES ($1, $2, $3, $4, $5, $6, $7)
                                RETURNING id;


-- name: InsertAllCommentData10 :one
INSERT INTO "PdfHighlightAnchor" (
            "uuid", "documentId", "owner", "page", "red", "green", "blue", "alpha", 
            "type", "text", "pageViewportWidth", "pageViewportHeight", 
            "threadId", "createdAt", "updatedAt"
        )
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
        RETURNING uuid;


-- name: InsertAllCommentData11 :one
INSERT INTO "PdfHighlightAnchor" (
            "documentId", "owner", "page", "red", "green", "blue", "alpha", 
            "type", "text", "pageViewportWidth", "pageViewportHeight", 
            "threadId", "createdAt", "updatedAt"
        )
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
        RETURNING uuid;


-- name: InsertAllCommentData12 :one
INSERT INTO "PdfHighlightRect" (
                            "top", "left", "width", "height", "pdfHighlightAnchorId"
                        )
                        VALUES ($1, $2, $3, $4, $5)
                        RETURNING id;


-- name: TryInsertCommentData :exec
SAVEPOINT comment_data_insertion;


-- name: TryInsertCommentData2 :exec
RELEASE SAVEPOINT comment_data_insertion;


-- name: TryInsertCommentData3 :exec
ROLLBACK TO SAVEPOINT comment_data_insertion;


-- name: SaveDocument :one
UPDATE "Document" SET "updatedAt" = NOW()
        WHERE id = $1
        RETURNING id as "document_id", owner, "fileType" as file_type, name as document_name,
        "branchedFromId" as branched_from_id, "branchedFromVersionId" as branched_from_version_id,
        "documentFamilyId" as document_family_id,
        "projectId" as project_id,
        "deletedAt"::timestamptz as "deleted_at";


-- name: SaveDocument2 :one
select name from "Project" where id = $1;


-- name: SaveDocument3 :one
INSERT INTO "DocumentInstance" ("documentId", "sha")
                VALUES ($1, $2)
                RETURNING id, sha, "createdAt"::timestamptz as created_at, "updatedAt"::timestamptz as updated_at;


-- name: SaveDocument4 :exec
INSERT INTO "DocumentInstanceModificationData" ("documentInstanceId", "modificationData")
                        VALUES ($1, $2);


-- name: SaveDocument5 :one
SELECT sub_type as "sub_type" FROM document_sub_type WHERE document_id = $1;

