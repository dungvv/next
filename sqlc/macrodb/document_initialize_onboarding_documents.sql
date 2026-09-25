-- name: CreateProjectTransaction :one
INSERT INTO "Project" ("name", "userId", "parentId", "createdAt", "updatedAt")
        VALUES ($1, $2, $3, NOW(), NOW())
        RETURNING id, name, "userId"::text as user_id, "createdAt"::timestamptz as created_at, "deletedAt"::timestamptz as deleted_at,
        "updatedAt"::timestamptz as updated_at, "parentId" as parent_id;


-- name: CreateOnboardingDocx :one
INSERT INTO "DocumentBom" ("documentId")
            VALUES ($1)
            RETURNING id;

