-- name: GetChatMessagesForSearchBackfill :many
SELECT
            c."id" as "chat_id",
            m.id as "message_id",
            c."userId" as "user_id",
            m."createdAt" as "created_at",
            m."updatedAt" as "updated_at"
        FROM
            "ChatMessage" m
        JOIN
            "Chat" c on c."id" = m."chatId"
        WHERE
            (
                $2::bool IS NULL
                OR ($2 AND c."deletedAt" IS NOT NULL)
                OR (NOT $2 AND c."deletedAt" IS NULL)
            )
            AND ($3::timestamptz IS NULL OR m."updatedAt" >= $3)
            AND ($4::timestamptz IS NULL OR m."updatedAt" < $4)
            AND (
                $5::timestamptz IS NULL
                OR (m."updatedAt", m.id) > ($5, $6::text)
            )
        ORDER BY m."updatedAt" ASC, m.id ASC
        LIMIT $1;


-- name: GetChatMessagesForSearchBackfillChatIds :many
SELECT
            c."id" as "chat_id",
            m.id as "message_id",
            c."userId" as "user_id",
            m."createdAt" as "created_at",
            m."updatedAt" as "updated_at"
        FROM
            "ChatMessage" m
        JOIN
            "Chat" c on c."id" = m."chatId"
        WHERE
            m."chatId" = ANY($1::text[])
            AND (
                $3::bool IS NULL
                OR ($3 AND c."deletedAt" IS NOT NULL)
                OR (NOT $3 AND c."deletedAt" IS NULL)
            )
            AND ($4::timestamptz IS NULL OR m."updatedAt" >= $4)
            AND ($5::timestamptz IS NULL OR m."updatedAt" < $5)
            AND (
                $6::timestamptz IS NULL
                OR (m."updatedAt", m.id) > ($6, $7::text)
            )
        ORDER BY m."updatedAt" ASC, m.id ASC
        LIMIT $2;


-- name: GetChatMessagesForSearchBackfillUserIds :many
SELECT
            c."id" as "chat_id",
            m.id as "message_id",
            c."userId" as "user_id",
            m."createdAt" as "created_at",
            m."updatedAt" as "updated_at"
        FROM
            "ChatMessage" m
        JOIN
            "Chat" c on c."id" = m."chatId"
        WHERE
            c."userId" = ANY($1::text[])
            AND (
                $3::bool IS NULL
                OR ($3 AND c."deletedAt" IS NOT NULL)
                OR (NOT $3 AND c."deletedAt" IS NULL)
            )
            AND ($4::timestamptz IS NULL OR m."updatedAt" >= $4)
            AND ($5::timestamptz IS NULL OR m."updatedAt" < $5)
            AND (
                $6::timestamptz IS NULL
                OR (m."updatedAt", m.id) > ($6, $7::text)
            )
        ORDER BY m."updatedAt" ASC, m.id ASC
        LIMIT $2;


-- name: GetChatMessageInfo :one
SELECT
            m.content as "content",
            c.name as "name",
            m.role as "role",
            c."deletedAt"::timestamptz as "deleted_at",
            c."userId" as "owner_user_id",
            m."createdAt" as "created_at",
            m."updatedAt" as "updated_at"
        FROM
            "ChatMessage" m
        JOIN
            "Chat" c on c."id" = m."chatId"
        WHERE
            m.id = $1 AND m."chatId" = $2;


-- name: GetChatsMetadataForUpdate :one
SELECT
            c.name
        FROM
            "Chat" c
        WHERE
            c.id = $1 AND c."deletedAt" IS NULL;


-- name: GetChatIdsByUserId :many
SELECT
            c."id" as "chat_id"
        FROM
            "Chat" c
        WHERE
            c."userId" = $1 AND c."deletedAt" IS NULL;


-- name: GetAllChatIdsWithUsersPaginated :many
SELECT
            id as "chat_id",
            "userId" as "user_id"
        FROM
            "Chat"
        WHERE
            "deletedAt" IS NULL
        ORDER BY
            "createdAt" DESC
        LIMIT $1
        OFFSET $2;


-- name: GetAllChatIdsPaginated :many
SELECT
            id as "chat_id"
        FROM
            "Chat"
        WHERE
            "deletedAt" IS NULL
        ORDER BY
            "createdAt" ASC
        LIMIT $1
        OFFSET $2;


-- name: GetChatHistoryInfo :many
SELECT
            c."id" as "item_id",
            c."createdAt" as "created_at",
            c."updatedAt" as "updated_at",
            c."deletedAt" as "deleted_at",
            uh."updatedAt" as "viewed_at",
            c."projectId" as "project_id",
            c."userId" as "user_id",
            c."name"
        FROM
            "Chat" c
        LEFT JOIN
            "UserHistory" uh ON uh."itemId" = c."id"
                AND uh."userId" = $1
                AND uh."itemType" = 'chat'
        WHERE
            c."id" = ANY($2::text[])
        ORDER BY
            c."updatedAt" DESC;

