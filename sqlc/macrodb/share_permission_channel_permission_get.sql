-- name: GetChannelSharePermissionsByChannelId :many
SELECT
            channel_id,
            share_permission_id,
            access_level as "access_level"
        FROM
            "ChannelSharePermission"
        WHERE
            channel_id = $1;

