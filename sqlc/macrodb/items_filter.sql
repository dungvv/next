-- name: FilterItemsByProjectIds :many
SELECT
                    d.id
                FROM
                    "Document" d
                WHERE
                    d."id" = ANY($1::text[])
                    AND d."projectId" = ANY($2::text[]);


-- name: FilterItemsByProjectIds2 :many
SELECT
                        c.id
                    FROM
                        "Chat" c
                    WHERE
                        c."id" = ANY($1::text[])
                        AND c."projectId" = ANY($2::text[]);


-- name: FilterItemsByOwnerIds :many
SELECT
                    d.id
                FROM
                    "Document" d
                WHERE
                    d."id" = ANY($1::text[])
                    AND d."owner" = ANY($2::text[]);


-- name: FilterItemsByOwnerIds2 :many
SELECT
                        c.id
                    FROM
                        "Chat" c
                    WHERE
                        c."id" = ANY($1::text[])
                        AND c."userId" = ANY($2::text[]);


-- name: FilterItemsByOwnerIds3 :many
SELECT
                        p.id
                    FROM
                        "Project" p
                    WHERE
                        p."id" = ANY($1::text[])
                        AND p."userId" = ANY($2::text[]);

