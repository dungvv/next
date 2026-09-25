-- name: GetOwnerAndDeleted :one
SELECT owner, "deletedAt" as deleted_at FROM "Document" WHERE id=$1;


-- name: GetOwnerAndDeleted2 :one
SELECT "userId" as user_id, "deletedAt" as deleted_at FROM "Chat" WHERE id=$1;


-- name: GetOwnerAndDeleted3 :one
SELECT "userId" as user_id, "deletedAt" as deleted_at FROM "Project" WHERE id=$1;

