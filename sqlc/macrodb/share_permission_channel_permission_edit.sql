-- name: UpsertChannelSharePermissions :exec
INSERT INTO "ChannelSharePermission" ("share_permission_id", "channel_id", "access_level")
        SELECT $1, channel_id, access_level::"AccessLevel"
        FROM (SELECT unnest($2::text[]) AS channel_id, unnest($3::text[]) AS access_level) AS t
        ON CONFLICT ("share_permission_id", "channel_id") 
        DO UPDATE SET "access_level" = EXCLUDED."access_level";


-- name: RemoveChannelSharePermissions :exec
DELETE FROM "ChannelSharePermission"
        WHERE "share_permission_id" = $1
        AND "channel_id" = ANY($2::text[]);

