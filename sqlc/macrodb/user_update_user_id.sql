-- name: UpdateLegacyUserId :exec
UPDATE "User"
            SET id = $1
            WHERE id = $2;


-- name: GetLegacyUsers :many
SELECT
            u.id AS user_id,
            u.email,
            COUNT(d.id) AS document_count
        FROM
            "User" u
        LEFT JOIN
            "Document" d ON u.id = d.owner AND d."fileType" IS DISTINCT FROM 'docx'
        WHERE
            u.id NOT LIKE 'macro|%'
        GROUP BY
            u.id, u.email
        ORDER BY
            document_count DESC;

