-- name: GetChatHistory :many
SELECT
            c.id as chat_id,
            c.name as chat_title,
            m.content::text as message_content,
            m."createdAt" as message_created_at,
            ma.entity_id::text as attachment_id
        FROM "Chat" c
        JOIN "ChatMessage" m ON m."chatId" = c.id
        LEFT JOIN "ChatAttachment" ma ON ma."messageId" = m.id
        WHERE c.id = $1
        ORDER BY m."createdAt" ASC;


-- name: GetChatHistoryForMessages :many
SELECT
            c.id as chat_id,
            c.name as chat_title,
            m.content::text as message_content,
            m."createdAt" as message_created_at,
            ma.entity_id::text as attachment_id
        FROM "Chat" c
        JOIN "ChatMessage" m ON m."chatId" = c.id
        LEFT JOIN "ChatAttachment" ma ON ma."messageId" = m.id
        WHERE m."id" = ANY($1::text[])
        ORDER BY m."createdAt" ASC;

