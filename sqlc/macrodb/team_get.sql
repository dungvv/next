-- name: GetUserTeams :many
SELECT
                t.id,
                t.name,
                t.owner_id
            FROM team t
            JOIN team_user tu ON t.id = tu.team_id
            WHERE tu.user_id = $1;

