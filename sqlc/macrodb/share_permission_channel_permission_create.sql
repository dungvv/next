-- name: InsertChannelSharePermission :exec
INSERT INTO "ChannelSharePermission" ("share_permission_id", "channel_id", "access_level")
            VALUES ($1, $2, $3);

