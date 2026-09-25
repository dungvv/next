-- name: DeleteMessageLabelsBatch :exec
DELETE FROM email_message_labels
        WHERE 
            "message_id" = ANY($1::uuid[]) 
            AND label_id = (
                SELECT id FROM email_labels
                WHERE link_id = $2 AND provider_label_id = $3
            );


-- name: DeleteAllMessageLabels :exec
DELETE FROM email_message_labels
        WHERE message_id = $1;


-- name: DeleteDbMessageLabels :exec
DELETE FROM email_message_labels
        WHERE message_id = $1
        AND label_id IN (
            SELECT id FROM email_labels
            WHERE link_id = $2 AND "provider_label_id" = ANY($3::text[])
        );


-- name: DeleteLabelsByProviderIds :exec
DELETE FROM email_message_labels
        WHERE label_id IN (
            SELECT id FROM email_labels
            WHERE link_id = $1 AND "provider_label_id" = ANY($2::text[])
        );


-- name: DeleteLabelsByProviderIds2 :exec
DELETE FROM email_labels
        WHERE link_id = $1 AND "provider_label_id" = ANY($2::text[]);


-- name: DeleteLabelById :exec
DELETE FROM email_message_labels
        WHERE label_id = $1;


-- name: DeleteLabelById2 :exec
DELETE FROM email_labels
        WHERE id = $1 AND link_id = $2;

