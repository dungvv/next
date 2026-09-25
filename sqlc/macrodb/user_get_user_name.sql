-- name: GetUserName :one
SELECT macro_user_id, first_name, last_name FROM macro_user_info WHERE macro_user_id = $1;


-- name: GetUserNames :many
SELECT 
                u.id as user_profile_id, 
                mui.first_name, 
                mui.last_name
            FROM macro_user_info mui
            JOIN "User" u ON mui.macro_user_id = u.macro_user_id
            WHERE u."id" = ANY($1::text[]);


-- name: GetUserNamesWithEmail :many
WITH requested_ids AS (
            SELECT DISTINCT id
            FROM (SELECT unnest($2::text[]) AS id) AS requested
        )
        SELECT
            req.id::text as "user_profile_id",
            CASE
                WHEN NULLIF(mui.first_name, 'N/A') IS NOT NULL
                  OR NULLIF(mui.last_name, 'N/A') IS NOT NULL
                THEN NULLIF(mui.first_name, 'N/A')
                ELSE SPLIT_PART(contact.name, ' ', 1)
            END::text as "first_name",
            CASE
                WHEN NULLIF(mui.first_name, 'N/A') IS NOT NULL
                  OR NULLIF(mui.last_name, 'N/A') IS NOT NULL
                THEN NULLIF(mui.last_name, 'N/A')
                ELSE CASE
                    WHEN POSITION(' ' IN contact.name) > 0
                    THEN NULLIF(TRIM(SUBSTRING(contact.name FROM POSITION(' ' IN contact.name) + 1)), '')
                    ELSE NULL
                END
            END::text as "last_name"
        FROM requested_ids req
        LEFT JOIN "User" u ON u.id = req.id
        LEFT JOIN macro_user_info mui ON mui.macro_user_id = u.macro_user_id
        LEFT JOIN LATERAL (
            SELECT ec.name
            FROM email_links li
            JOIN email_contacts ec
                ON ec.link_id = li.id
                AND ec.email_address = REPLACE(req.id, 'macro|', '')
                AND ec.name IS NOT NULL
            WHERE li.macro_id = $1
              AND NULLIF(mui.first_name, 'N/A') IS NULL
              AND NULLIF(mui.last_name, 'N/A') IS NULL
            LIMIT 1
        ) contact ON TRUE
        WHERE u.id IS NOT NULL OR contact.name IS NOT NULL;

