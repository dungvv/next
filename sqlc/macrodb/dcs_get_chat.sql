-- name: GetChatName :one
SELECT name
        FROM "Chat"
        WHERE id = $1;


-- name: GetChatDb :one
SELECT
              c.id,
              c.name,
              c.model,
              c."userId" as "user_id",
              c."createdAt"::timestamptz as "created_at",
              c."updatedAt"::timestamptz as "updated_at",
              c."deletedAt"::timestamptz as "deleted_at",
              c."projectId" as "project_id",
              c."tokenCount" as "token_count",
              c."isPersistent" as "is_persistent"
          FROM "Chat" c WHERE c.id = $1;


-- name: RawAttachments :many
SELECT
              ca.id,
              ca."chatId" as "chat_id",
              ca."entity_id"::TEXT as "attachment_id",
              ca."entity_type" as "attachment_type",
              ca."messageId" as "message_id"
          FROM
              "ChatAttachment" ca
          WHERE ca."chatId" = $1
          ORDER BY ca.id ASC;


-- name: GetMessages :many
SELECT
            cm.id AS "id",
            cm.content,
            cm.role,
            cm.model,
            COALESCE(
                (
                    SELECT json_agg(
                        json_build_object(
                            'entity_type', ca."entity_type",
                            'entity_id', ca."entity_id"::TEXT
                        )
                    )
                    FROM "ChatAttachment" ca
                    WHERE ca."messageId" = cm.id
                ),
                '[]'::jsonb
            )::jsonb AS attachments
        FROM
            "ChatMessage" cm
        WHERE
            cm."chatId" = $1
        ORDER BY
            cm."createdAt" ASC;


-- name: GetWebCitations :many
SELECT
            "messageId" as "message_id",
            "url",
            "title",
            "description",
            "favicon_url",
            "image_url"
        FROM "WebAnnotations" wa
        INNER JOIN "ChatMessage" cm ON cm.id = wa."messageId"
        WHERE cm."chatId" = $1;

