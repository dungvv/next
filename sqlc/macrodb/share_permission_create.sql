-- name: CreateSharePermission :one
INSERT INTO "SharePermission" (
                "linkShare",
                "linkShareAccessLevel",
                "createdAt",
                "updatedAt"
            )
            VALUES ($1, $2, NOW(), NOW())
            RETURNING id;


-- name: CreateDocumentPermission :exec
INSERT INTO "DocumentPermission" ("documentId", "sharePermissionId")
            VALUES ($1, $2);


-- name: CreateProjectPermission :exec
INSERT INTO "ProjectPermission" ("projectId", "sharePermissionId")
            VALUES ($1, $2);


-- name: CreateChatPermission :exec
INSERT INTO "ChatPermission" ("chatId", "sharePermissionId")
            VALUES ($1, $2);


-- name: CreateThreadPermission :exec
INSERT INTO "EmailThreadPermission" ("threadId", "sharePermissionId", "userId")
            VALUES ($1, $2, $3);

