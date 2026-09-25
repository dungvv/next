-- name: ShareLinkSharedDocumentWithMentionedUsers :exec
INSERT INTO entity_access (entity_id, entity_type, source_id, source_type, access_level)
        SELECT dp."documentId"::uuid, 'document', u.user_id, 'user', sp."linkShareAccessLevel"::"AccessLevel"
        FROM "DocumentPermission" dp
        JOIN "SharePermission" sp ON sp.id = dp."sharePermissionId"
        CROSS JOIN UNNEST($2::text[]) AS u(user_id)
        WHERE dp."documentId" = $1
          AND sp."linkShare" IN ('PUBLIC', 'TEAM')
          AND sp."linkShareAccessLevel" IS NOT NULL
        ON CONFLICT (entity_id, entity_type, source_id, source_type)
        WHERE granted_from_project_id IS NULL
        DO NOTHING;

