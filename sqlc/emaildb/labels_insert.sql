-- name: InsertMessageLabel :one
WITH label_lookup AS (
            SELECT id FROM email_labels
            WHERE link_id = $2 AND provider_label_id = $3
        )
        INSERT INTO email_message_labels (message_id, label_id)
        SELECT $1, id FROM label_lookup
        ON CONFLICT (message_id, label_id) DO NOTHING
        RETURNING (SELECT COUNT(*) FROM label_lookup) AS label_found;


-- name: InsertMessageLabelsBatch :exec
INSERT INTO email_message_labels (message_id, label_id)
        SELECT
            unnested_message_id,
            l.id
        FROM (SELECT unnest($1::uuid[]) AS unnested_message_id) AS t
        CROSS JOIN
            email_labels l
        WHERE
            l.link_id = $2 AND l.provider_label_id = $3
        ON CONFLICT (message_id, label_id) DO NOTHING;


-- name: InsertOrUpdateLabels :exec
WITH input_rows (
            id,
            link_id,
            provider_label_id,
            name,
            message_list_visibility,
            label_list_visibility,
            type
        ) AS (
           SELECT unnest($1::uuid[]), unnest($2::uuid[]), unnest($3::text[]), unnest($4::text[]), unnest($5::email_message_list_visibility_enum[]), unnest($6::email_label_list_visibility_enum[]), unnest($7::email_label_type_enum[])
        )
        INSERT INTO email_labels (
            id,
            link_id,
            provider_label_id,
            name,
            message_list_visibility,
            label_list_visibility,
            type
        )
        SELECT
            id,
            link_id,
            provider_label_id,
            name,
            message_list_visibility,
            label_list_visibility,
            type
        FROM input_rows
        ON CONFLICT (link_id, provider_label_id) DO UPDATE
        SET
            name = EXCLUDED.name,
            message_list_visibility = EXCLUDED.message_list_visibility,
            label_list_visibility = EXCLUDED.label_list_visibility,
            type = EXCLUDED.type;


-- name: InsertLabel :one
INSERT INTO email_labels (
            id,
            link_id,
            provider_label_id,
            name,
            message_list_visibility,
            label_list_visibility,
            type
        )
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (link_id, provider_label_id) DO UPDATE
        SET
            name = EXCLUDED.name,
            message_list_visibility = EXCLUDED.message_list_visibility,
            label_list_visibility = EXCLUDED.label_list_visibility,
            type = EXCLUDED.type
        RETURNING 
            id,
            link_id,
            provider_label_id,
            name,
            created_at,
            message_list_visibility as "message_list_visibility",
            label_list_visibility as "label_list_visibility",
            type as "type_";


-- name: InsertMessageLabels :many
SELECT id, provider_label_id
        FROM email_labels
        WHERE link_id = $1 AND "provider_label_id" = ANY($2::text[]);


-- name: InsertMessageLabels2 :exec
DELETE FROM email_message_labels
        WHERE message_id = $1
        AND label_id NOT IN (
            SELECT UNNEST($2::uuid[])
        );


-- name: InsertMessageLabels3 :exec
INSERT INTO email_message_labels (message_id, label_id)
        SELECT unnest($1::uuid[]), unnest($2::uuid[])
        ON CONFLICT (message_id, label_id) DO NOTHING;

