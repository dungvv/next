-- name: DeleteUserEntityAccessByItem :exec
DELETE FROM "entity_access"
        WHERE "entity_id" = $1 AND "entity_type" = $2;


-- name: DeleteUserEntityAccessBulk :exec
DELETE FROM "entity_access"
        WHERE ("entity_id" = ANY($1::uuid[]) AND entity_type = $2)
        OR "granted_from_project_id" = ANY($3::text[]);


-- name: DeleteUserEntityAccessBulk2 :exec
DELETE FROM "entity_access"
        WHERE "entity_id" = ANY($1::uuid[]) AND entity_type = $2;

