package dss

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AccessLevel mirrors models_permissions::share_permission::access_level::
// AccessLevel. Ordering matters: View < Comment < Edit < Owner.
type AccessLevel int

const (
	AccessLevelNone    AccessLevel = 0
	AccessLevelView    AccessLevel = 1
	AccessLevelComment AccessLevel = 2
	AccessLevelEdit    AccessLevel = 3
	AccessLevelOwner   AccessLevel = 4
)

func (l AccessLevel) String() string {
	switch l {
	case AccessLevelView:
		return "view"
	case AccessLevelComment:
		return "comment"
	case AccessLevelEdit:
		return "edit"
	case AccessLevelOwner:
		return "owner"
	default:
		return ""
	}
}

func parseAccessLevel(s string) AccessLevel {
	switch strings.ToLower(s) {
	case "view":
		return AccessLevelView
	case "comment":
		return AccessLevelComment
	case "edit":
		return AccessLevelEdit
	case "owner":
		return AccessLevelOwner
	default:
		return AccessLevelNone
	}
}

// EntityType mirrors model_entity::EntityType for the entities DSS checks.
type EntityType string

const (
	EntityTypeDocument EntityType = "document"
	EntityTypeProject  EntityType = "project"
	EntityTypeChat     EntityType = "chat"
)

// ErrUnauthorized mirrors AccessError::Unauthorized.
var ErrUnauthorized = errors.New("unauthorized")

// ErrNotFound mirrors DocumentError::NotFound / missing entity rows.
var ErrNotFound = errors.New("not found")

// accessChecker ports the core of crates/entity_access: the
// entity_access + SharePermission union queries that compute a caller's
// highest access level for a document or project.
type accessChecker struct {
	pool *pgxpool.Pool
}

// userSourceIDs ports get_user_source_ids: the caller's channel ids, team ids,
// and their own user id — the source_id set matched against entity_access.
// Empty when there is no authenticated user (public-access path).
func (a *accessChecker) userSourceIDs(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	rows, err := a.pool.Query(ctx, `
		SELECT cp.channel_id::text FROM comms_channel_participants cp
			WHERE cp.user_id = $1 AND cp.left_at IS NULL
		UNION ALL
		SELECT t.team_id::text FROM team_user t
			WHERE t.user_id = $1
		UNION ALL
		SELECT $1
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id *string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != nil {
			ids = append(ids, *id)
		}
	}
	return ids, rows.Err()
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
		default:
			return false
		}
	}
	return true
}

// documentAccessLevel ports queries::document_access::get_document_access
// (UUID ids) and get_legacy_document_access (non-UUID ids).
// Returns AccessLevelNone when the caller has no grant.
func (a *accessChecker) documentAccessLevel(ctx context.Context, documentID string, sourceIDs []string, userID string) (AccessLevel, error) {
	if !isUUID(documentID) {
		return a.legacyDocumentAccessLevel(ctx, documentID, sourceIDs, userID)
	}
	if len(sourceIDs) == 0 {
		var level *string
		err := a.pool.QueryRow(ctx, `
			SELECT share_permission."linkShareAccessLevel"
			FROM "SharePermission" share_permission
			JOIN "DocumentPermission" document_permission
			  ON document_permission."sharePermissionId" = share_permission.id
			WHERE share_permission."linkShare" = 'PUBLIC'
			  AND share_permission."linkShareAccessLevel" IS NOT NULL
			  AND document_permission."documentId" = $1
		`, documentID).Scan(&level)
		if errors.Is(err, pgx.ErrNoRows) {
			return AccessLevelNone, nil
		}
		if err != nil {
			return AccessLevelNone, err
		}
		if level == nil {
			return AccessLevelNone, nil
		}
		return parseAccessLevel(*level), nil
	}

	rows, err := a.pool.Query(ctx, `
		SELECT access_level FROM (
			SELECT access_level::text FROM entity_access
			WHERE entity_id::text = $1
			AND entity_type = 'document'
			AND source_id = ANY($2)

			UNION ALL

			SELECT share_permission."linkShareAccessLevel"::text AS access_level
			FROM "Document" document
			JOIN "DocumentPermission" document_permission
			  ON document_permission."documentId" = document.id
			JOIN "SharePermission" share_permission
			  ON share_permission.id = document_permission."sharePermissionId"
			WHERE document.id = $3
			  AND share_permission."linkShareAccessLevel" IS NOT NULL
			  AND (
				  share_permission."linkShare" = 'PUBLIC'
				  OR (
					  share_permission."linkShare" = 'TEAM'
					  AND EXISTS (
						  SELECT 1
						  FROM team_user owner_team
						  WHERE owner_team.user_id = document.owner
							AND owner_team.team_id::text = ANY($2)
					  )
				  )
			  )

			UNION ALL

			-- email-attachment documents inherit access from a linked thread
			SELECT CASE
				WHEN l.macro_id = $4
				  OR EXISTS (
					  SELECT 1
					  FROM macro_user_links mul
					  WHERE mul.link_id = l.id
						AND mul.primary_macro_id = $4
				  )
				THEN 'edit'
				ELSE 'view'
			END AS access_level
			FROM document_email de
			JOIN email_attachments ea ON ea.id = de.email_attachment_id
			JOIN email_messages em ON em.id = ea.message_id
			JOIN email_threads t ON t.id = em.thread_id
			JOIN email_links l ON l.id = t.link_id
			WHERE de.document_id = $3
			  AND (
				  l.macro_id = $4
				  OR EXISTS (
					  SELECT 1
					  FROM macro_user_links mul
					  WHERE mul.link_id = l.id
						AND mul.primary_macro_id = $4
				  )
				  OR EXISTS (
					  SELECT 1
					  FROM entity_access thread_access
					  WHERE thread_access.entity_id = t.id
						AND thread_access.entity_type = 'email_thread'
						AND thread_access.source_id = ANY($2)
				  )
			  )

			UNION ALL

			-- a file attached to a document discussion is visible to
			-- current viewers of that document
			SELECT 'view' AS access_level
			FROM comms_attachments a
			JOIN comms_messages m ON m.id = a.message_id
			JOIN comms_message_threads mt ON mt.root_id = COALESCE(m.thread_id, m.id)
			JOIN "Document" parent_doc
			  ON m.parent_entity_type = 'document' AND parent_doc.id = m.parent_entity_id
			LEFT JOIN "DocumentPermission" dp ON dp."documentId" = parent_doc.id
			LEFT JOIN "SharePermission" sp ON sp.id = dp."sharePermissionId"
			WHERE a.entity_type = 'document'
			  AND a.entity_id = $3
			  AND m.deleted_at IS NULL
			  AND mt.deleted_at IS NULL
			  AND parent_doc."deletedAt" IS NULL
			  AND (
				  parent_doc.owner = $4
				  OR EXISTS (
					  SELECT 1
					  FROM entity_access pa
					  WHERE pa.entity_type = 'document'
						AND pa.entity_id::text = parent_doc.id
						AND pa.source_id = ANY($2)
				  )
				  OR (
					  sp."linkShareAccessLevel" IS NOT NULL
					  AND (
						  sp."linkShare" = 'PUBLIC'
						  OR (
							  sp."linkShare" = 'TEAM'
							  AND EXISTS (
								  SELECT 1
								  FROM team_user tu
								  WHERE tu.user_id = parent_doc.owner
									AND tu.team_id::text = ANY($2)
							  )
						  )
					  )
				  )
			  )
		) AS combined_access
	`, documentID, sourceIDs, documentID, userID)
	if err != nil {
		return AccessLevelNone, err
	}
	defer rows.Close()
	return maxAccessLevel(rows)
}

// legacyDocumentAccessLevel ports get_legacy_document_access for historical
// non-UUID document ids.
func (a *accessChecker) legacyDocumentAccessLevel(ctx context.Context, documentID string, sourceIDs []string, userID string) (AccessLevel, error) {
	var uid *string
	if userID != "" {
		uid = &userID
	}
	rows, err := a.pool.Query(ctx, `
		WITH RECURSIVE parent_projects AS (
			SELECT p.id, p."parentId", p."userId"
			FROM "Project" p
			JOIN "Document" d ON d."projectId" = p.id
			WHERE d.id = $1 AND d."deletedAt" IS NULL AND p."deletedAt" IS NULL
			UNION
			SELECT p.id, p."parentId", p."userId"
			FROM "Project" p
			JOIN parent_projects child ON p.id = child."parentId"
			WHERE p."deletedAt" IS NULL
		), document_permissions AS (
			SELECT d.owner, sp.id, sp."linkShare", sp."linkShareAccessLevel"
			FROM "Document" d
			LEFT JOIN "DocumentPermission" dp ON dp."documentId" = d.id
			LEFT JOIN "SharePermission" sp ON sp.id = dp."sharePermissionId"
			WHERE d.id = $1 AND d."deletedAt" IS NULL
		)
		SELECT 'owner'::text AS "level" FROM document_permissions WHERE owner = $2
		UNION ALL
		SELECT "linkShareAccessLevel"::text FROM document_permissions p
		WHERE "linkShareAccessLevel" IS NOT NULL
		  AND (
			  "linkShare" = 'PUBLIC'
			  OR (
				  "linkShare" = 'TEAM'
				  AND EXISTS (
					  SELECT 1 FROM team_user t
					  WHERE t.user_id = p.owner AND t.team_id::text = ANY($3)
				  )
			  )
		  )
		UNION ALL
		SELECT c.access_level::text FROM document_permissions p
		JOIN "ChannelSharePermission" c ON c.share_permission_id = p.id
		WHERE c.channel_id = ANY($3)
		UNION ALL
		SELECT 'edit'::text FROM parent_projects WHERE "userId" = $2
		UNION ALL
		SELECT a.access_level::text FROM entity_access a
		JOIN parent_projects p ON a.entity_id::text = p.id
		WHERE a.entity_type = 'project' AND a.source_id = ANY($3)
	`, documentID, uid, sourceIDs)
	if err != nil {
		return AccessLevelNone, err
	}
	defer rows.Close()
	return maxAccessLevel(rows)
}

// projectAccessLevel ports queries::project_access::get_project_access.
func (a *accessChecker) projectAccessLevel(ctx context.Context, projectID string, sourceIDs []string) (AccessLevel, error) {
	if len(sourceIDs) == 0 {
		var level *string
		err := a.pool.QueryRow(ctx, `
			SELECT sp."linkShareAccessLevel"
			FROM "SharePermission" sp
			WHERE sp."linkShare" = 'PUBLIC'
			AND sp."linkShareAccessLevel" IS NOT NULL
			AND sp.id IN (
				SELECT "sharePermissionId" FROM "ProjectPermission" WHERE "projectId" = $1
			)
		`, projectID).Scan(&level)
		if errors.Is(err, pgx.ErrNoRows) {
			return AccessLevelNone, nil
		}
		if err != nil {
			return AccessLevelNone, err
		}
		if level == nil {
			return AccessLevelNone, nil
		}
		return parseAccessLevel(*level), nil
	}

	rows, err := a.pool.Query(ctx, `
		SELECT access_level FROM (
			SELECT access_level::text FROM entity_access
			WHERE entity_id::text = $1
			AND entity_type = 'project'
			AND source_id = ANY($2)

			UNION ALL

			SELECT sp."linkShareAccessLevel"::text AS access_level
			FROM "SharePermission" sp
			WHERE sp."linkShareAccessLevel" IS NOT NULL
			AND sp.id IN (
				SELECT "sharePermissionId" FROM "ProjectPermission" WHERE "projectId" = $3
			)
			AND (
				sp."linkShare" = 'PUBLIC'
				OR (
					sp."linkShare" = 'TEAM'
					AND EXISTS (
						SELECT 1
						FROM "Project" p
						JOIN team_user owner_team ON owner_team.user_id = p."userId"
						WHERE p.id = $3
						AND owner_team.team_id::text = ANY($2)
					)
				)
			)
		) AS combined_access
	`, projectID, sourceIDs, projectID)
	if err != nil {
		return AccessLevelNone, err
	}
	defer rows.Close()
	return maxAccessLevel(rows)
}

func maxAccessLevel(rows pgx.Rows) (AccessLevel, error) {
	max := AccessLevelNone
	for rows.Next() {
		var s *string
		if err := rows.Scan(&s); err != nil {
			return AccessLevelNone, err
		}
		if s == nil {
			continue
		}
		if l := parseAccessLevel(*s); l > max {
			max = l
		}
	}
	return max, rows.Err()
}

// accessLevelFor resolves the caller's effective access level for a document
// or project. Owner short-circuits to owner. Mirrors
// EntityAccessService::get_access_level for Document/Project.
func (s *Service) accessLevelFor(ctx context.Context, caller Caller, entityType EntityType, entityID string) (AccessLevel, error) {
	userID := caller.UserID
	if caller.Internal && userID == internalUserID {
		// Internal service callers get full access (macro_authorization
		// Internal bypass).
		return AccessLevelOwner, nil
	}
	sourceIDs, err := s.access.userSourceIDs(ctx, userID)
	if err != nil {
		return AccessLevelNone, err
	}
	switch entityType {
	case EntityTypeDocument:
		return s.access.documentAccessLevel(ctx, entityID, sourceIDs, userID)
	case EntityTypeProject:
		return s.access.projectAccessLevel(ctx, entityID, sourceIDs)
	default:
		return AccessLevelNone, nil
	}
}

// requireAccess mirrors check_access: returns the level or ErrUnauthorized.
func (s *Service) requireAccess(ctx context.Context, caller Caller, entityType EntityType, entityID string, min AccessLevel) (AccessLevel, error) {
	level, err := s.accessLevelFor(ctx, caller, entityType, entityID)
	if err != nil {
		return AccessLevelNone, err
	}
	if level < min {
		return AccessLevelNone, ErrUnauthorized
	}
	return level, nil
}
