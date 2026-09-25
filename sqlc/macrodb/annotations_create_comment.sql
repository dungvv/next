-- name: CreateDocumentComment :one
UPDATE "Thread" t
                SET "updatedAt" = NOW(), "metadata" = $3
                WHERE t."documentId" = $1 AND t."deletedAt" IS NULL AND t.id = $2
                RETURNING
                    t.id as thread_id, 
                    t.resolved, 
                    t."documentId" as document_id, 
                    t."createdAt"::timestamptz as created_at, 
                    t."updatedAt"::timestamptz as updated_at, 
                    t."deletedAt"::timestamptz as deleted_at, 
                    t.metadata, 
                    t.owner;


-- name: CreateDocumentComment2 :one
UPDATE "Thread" t
                SET "updatedAt" = NOW()
                WHERE t."documentId" = $1 AND t."deletedAt" IS NULL AND t.id = $2
                RETURNING
                    t.id as thread_id, 
                    t.resolved, 
                    t."documentId" as document_id, 
                    t."createdAt"::timestamptz as created_at, 
                    t."updatedAt"::timestamptz as updated_at, 
                    t."deletedAt"::timestamptz as deleted_at, 
                    t.metadata, 
                    t.owner;


-- name: CreateDocumentComment3 :one
INSERT INTO "Thread" AS t 
                ("owner", "documentId", "createdAt", "updatedAt", "metadata")
                VALUES ($1, $2, NOW(), NOW(), $3)
                RETURNING
                    t.id as thread_id, 
                    t.resolved, 
                    t."documentId" as document_id, 
                    t."createdAt"::timestamptz as created_at, 
                    t."updatedAt"::timestamptz as updated_at, 
                    t."deletedAt"::timestamptz as deleted_at, 
                    t.metadata, 
                    t.owner;


-- name: CreateDocumentComment4 :one
INSERT INTO "Comment" AS c 
        ("threadId", "owner", "text", "metadata")
        VALUES ($1, $2, $3, $4)
        RETURNING
            c.id as comment_id, 
            c."threadId" as thread_id, 
            c.owner, 
            c.sender, 
            c.text, 
            c.metadata, 
            c."createdAt"::timestamptz as created_at, 
            c."updatedAt"::timestamptz as updated_at, 
            c."deletedAt"::timestamptz as deleted_at, 
            c.order;

