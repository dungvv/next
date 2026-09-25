-- name: UpdateDbMessageReplyingToId :one
UPDATE email_messages
        SET
            replying_to_id = $1,
            updated_at = NOW()
        WHERE
            id = $2
            AND link_id = $3
        RETURNING id;


-- name: UpdateDbMessagesReplyingToIds :exec
UPDATE email_messages
        SET
            replying_to_id = update_data.new_replying_to_id,
            updated_at = NOW()
        FROM (SELECT unnest($1::uuid[]) AS message_id, unnest($2::uuid[]) AS new_replying_to_id) AS update_data
        WHERE
            email_messages.id = update_data.message_id
            AND email_messages.link_id = $3;

