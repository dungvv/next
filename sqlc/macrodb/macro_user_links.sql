-- name: InsertEdge :exec
INSERT INTO macro_user_links (primary_macro_id, child_macro_id, link_id)
            VALUES ($1, $2, $3)
            ON CONFLICT (primary_macro_id, child_macro_id, link_id) DO NOTHING;


-- name: DeleteEdge :exec
DELETE FROM macro_user_links
            WHERE primary_macro_id = $1
              AND child_macro_id = $2
              AND link_id = $3;


-- name: EdgeExists :one
SELECT EXISTS(
                SELECT 1
                FROM macro_user_links
                WHERE primary_macro_id = $1
                  AND child_macro_id = $2
                  AND link_id = $3
            ) AS "exists";


-- name: ChildrenForPrimary :many
SELECT DISTINCT child_macro_id
            FROM macro_user_links
            WHERE primary_macro_id = $1;


-- name: GetPrimariesForChild :many
SELECT DISTINCT primary_macro_id
            FROM macro_user_links
            WHERE child_macro_id = $1;


-- name: GetPrimariesForLink :many
SELECT primary_macro_id
            FROM macro_user_links
            WHERE child_macro_id = $1
              AND link_id = $2;

