-- name: GetUserQuota :one
SELECT
            COUNT(DISTINCT cm.id) AS ai_chat_messages,
            COUNT(DISTINCT d.id) AS documents
        FROM "User" u
        LEFT JOIN "Chat" c ON c."userId" = u.id AND c."deletedAt" IS NULL
        LEFT JOIN "ChatMessage" cm ON cm."chatId" = c.id AND cm.role = 'user'
        LEFT JOIN "Document" d ON d."owner" = u.id AND d."deletedAt" IS NULL
        WHERE u.id = $1;

