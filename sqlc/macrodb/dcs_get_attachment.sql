-- name: GetAttachment :one
SELECT
                ca.id,
                ca."entity_type" as "attachment_type",
                ca."entity_id"::TEXT as "attachment_id",
                ca."chatId" as "chat_id",
                ca."messageId" as "message_id"
            FROM
                "ChatAttachment" ca
            WHERE
                ca."id" = $1
            LIMIT 1;

