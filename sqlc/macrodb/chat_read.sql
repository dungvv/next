-- name: GetUserMessages :many
SELECT 
      m.id 
    FROM 
      "ChatMessage" m
    JOIN 
      "Chat" c on c."id" = m."chatId"
    WHERE
      "userId" = $1
    AND
      m."updatedAt" <= $2
    AND 
      m."updatedAt" >= $3
    AND 
      "role" = 'user'
    
    ORDER BY 
      m."updatedAt" DESC
    LIMIT 
      $4
    OFFSET $5;

