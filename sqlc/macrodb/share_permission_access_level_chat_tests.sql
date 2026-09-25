-- name: ConfigureTeamLinkAccess :exec
INSERT INTO team (id, name, owner_id)
        VALUES
            ('dddddddd-dddd-dddd-dddd-000000000001', 'Chat owner team', 'user-1'),
            ('dddddddd-dddd-dddd-dddd-000000000002', 'Other team', 'user-2');


-- name: ConfigureTeamLinkAccess2 :exec
INSERT INTO team_user (user_id, team_id, team_role)
        VALUES
            ('user-1', 'dddddddd-dddd-dddd-dddd-000000000001', 'owner'),
            ('user-3', 'dddddddd-dddd-dddd-dddd-000000000001', 'member'),
            ('user-2', 'dddddddd-dddd-dddd-dddd-000000000002', 'owner'),
            ('user-public-access-only', 'dddddddd-dddd-dddd-dddd-000000000002', 'member');


-- name: ConfigureTeamLinkAccess3 :exec
UPDATE "SharePermission"
        SET "linkShare" = 'TEAM', "linkShareAccessLevel" = 'comment'
        WHERE id = 'sp-public-edit';


-- name: ConfigureTeamLinkAccess4 :exec
INSERT INTO entity_access (
            entity_id,
            entity_type,
            source_id,
            source_type,
            access_level
        )
        VALUES (
            'cccccccc-cccc-cccc-cccc-000000000001',
            'chat',
            'user-3',
            'user',
            'view'
        );

