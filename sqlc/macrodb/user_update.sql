-- name: UpsertMacroUserId :exec
UPDATE "User"
        SET macro_user_id = $2
        WHERE id = $1;


-- name: MigrateMacroUserInfo :one
SELECT
            industry,
            title,
            "firstName" as first_name,
            "lastName" as last_name,
            "profilePicture" as profile_picture,
            "profilePictureHash" as profile_picture_hash
        FROM "User"
        WHERE id = $1;


-- name: MigrateMacroUserInfo2 :exec
INSERT INTO macro_user_info (macro_user_id, industry, title, first_name, last_name, profile_picture, profile_picture_hash)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (macro_user_id) DO NOTHING;

