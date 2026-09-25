-- name: UpdateProfilePicture :exec
INSERT INTO macro_user_info (macro_user_id, profile_picture, profile_picture_hash)
        VALUES ($1, $2, $3)
        ON CONFLICT (macro_user_id)
        DO UPDATE SET 
            profile_picture = EXCLUDED.profile_picture,
            profile_picture_hash = EXCLUDED.profile_picture_hash;


-- name: GetProfilePictures :many
SELECT 
            u.id as user_profile_id, 
            mu.id as macro_user_id
        FROM macro_user mu
        JOIN "User" u ON mu.id = u.macro_user_id
        WHERE u."id" = ANY($1::text[]);


-- name: GetProfilePictures2 :many
SELECT macro_user_id, profile_picture as "profile_picture", profile_picture_hash FROM macro_user_info
        WHERE "macro_user_id" = ANY($1::uuid[]) and profile_picture IS NOT NULL;

