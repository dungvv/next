-- name: Update :exec
INSERT INTO macro_user_info (macro_user_id, first_name, last_name)
        VALUES ($1, $2, $3)
        ON CONFLICT (macro_user_id)
        DO UPDATE SET 
            first_name = COALESCE(EXCLUDED.first_name, macro_user_info.first_name),
            last_name = COALESCE(EXCLUDED.last_name, macro_user_info.last_name);

