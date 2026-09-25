-- name: FetchMessageLabel :one
SELECT ml.message_id, ml.label_id
        FROM email_message_labels ml
        JOIN email_labels l ON ml.label_id = l.id
        WHERE ml.message_id = $1
        AND l.provider_label_id = $2
        AND l.link_id = $3;


-- name: FetchMessageLabels :many
SELECT
            l.id,
            l.link_id,
            l.provider_label_id,
            l.name,
            l.created_at,
            l.message_list_visibility as "message_list_visibility",
            l.label_list_visibility as "label_list_visibility",
            l.type as "type_"
        FROM email_message_labels ml
        JOIN email_labels l ON ml.label_id = l.id
        WHERE ml.message_id = $1
        ORDER BY l.name;


-- name: FetchMessageLabelsInBulk :many
SELECT
            ml.message_id,
            l.id,
            l.link_id,
            l.provider_label_id,
            l.name,
            l.created_at,
            l.message_list_visibility as "message_list_visibility",
            l.label_list_visibility as "label_list_visibility",
            l.type as "type_"
        FROM email_message_labels ml
        JOIN email_labels l ON ml.label_id = l.id
        WHERE
            ml."message_id" = ANY($1::uuid[])
        ORDER BY ml.message_id;


-- name: FindMissingProviderLabels :many
SELECT provider_label_id
        FROM email_labels
        WHERE link_id = $1 AND "provider_label_id" = ANY($2::text[]);


-- name: FetchLabelsByLinkId :many
SELECT 
            id, 
            link_id, 
            provider_label_id, 
            name, 
            created_at,
            message_list_visibility as "message_list_visibility",
            label_list_visibility as "label_list_visibility",
            type as "type_"
        FROM email_labels
        WHERE link_id = $1
        ORDER BY name;


-- name: FetchLabelById :one
SELECT 
            id, 
            link_id, 
            provider_label_id, 
            name, 
            created_at,
            message_list_visibility as "message_list_visibility",
            label_list_visibility as "label_list_visibility",
            type as "type_"
        FROM email_labels
        WHERE id = $1 AND link_id = $2;

