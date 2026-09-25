package dss

// projects.go ports the basic projects endpoints from
// crates/projects/src/inbound/axum_router.rs.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
)

// Project mirrors model::project::Project (camelCase).
type Project struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	UserID    Owner      `json:"userId"`
	ParentID  *string    `json:"parentId,omitempty"`
	CreatedAt *time.Time `json:"createdAt"`
	UpdatedAt *time.Time `json:"updatedAt"`
	DeletedAt *time.Time `json:"deletedAt"`
}

// ensureProjectExists mirrors the ensure_project_exists middleware: 404 when
// the project row is missing.
func (s *Service) ensureProjectExists(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chiParam(r, "id")
		_, err := s.q.GetBasicProject(r.Context(), id)
		if isNoRows(err) {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("project with id %q was not found", id))
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get project")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// getProjects mirrors GET /projects — all non-deleted projects owned by the
// caller (Rust get_projects is owner-scoped).
func (s *Service) getProjects(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, name, "userId", "parentId", "createdAt"::timestamptz,
		       "updatedAt"::timestamptz, "deletedAt"::timestamptz
		FROM "Project"
		WHERE "userId" = $1 AND "deletedAt" IS NULL
		ORDER BY "updatedAt" DESC
	`, caller.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get projects")
		return
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.UserID, &p.ParentID,
			&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get projects")
			return
		}
		projects = append(projects, p)
	}
	writeOK(w, projects)
}

// createProject mirrors POST /projects — inserts the project, share
// permission, project permission, and the owner entity_access grant.
func (s *Service) createProject(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req struct {
		Name            string  `json:"name"`
		ProjectParentID *string `json:"projectParentId"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	ctx := r.Context()
	if req.ProjectParentID != nil && *req.ProjectParentID != "" {
		if _, err := s.requireAccess(ctx, caller, EntityTypeProject, *req.ProjectParentID, AccessLevelEdit); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "unable to check parent project access")
			return
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create project")
		return
	}
	defer tx.Rollback(ctx)

	projectID := uuid.Must(uuid.NewV7()).String()
	var createdAt, updatedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `
		INSERT INTO "Project" (id, name, "userId", "parentId", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		RETURNING "createdAt"::timestamptz, "updatedAt"::timestamptz
	`, projectID, req.Name, caller.UserID, pgText(req.ProjectParentID)).
		Scan(&createdAt, &updatedAt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create project")
		return
	}
	var sharePermID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO "SharePermission" ("createdAt", "updatedAt")
		VALUES (NOW(), NOW()) RETURNING id
	`).Scan(&sharePermID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create project")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "ProjectPermission" ("projectId", "sharePermissionId") VALUES ($1, $2)
	`, projectID, sharePermID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create project")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO entity_access (entity_id, entity_type, source_id, source_type, access_level)
		VALUES ($1::uuid, 'project', $2, 'user', 'owner')
	`, projectID, caller.UserID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create project")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO entity (id, entity_type, owner_type, owner_id)
		VALUES ($1::uuid, 'project', 'user', $2)
		ON CONFLICT (id) DO NOTHING
	`, projectID, caller.UserID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create project")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "UserHistory" ("userId", "itemId", "itemType", "createdAt", "updatedAt")
		VALUES ($1, $2, 'project', NOW(), NOW())
		ON CONFLICT ("userId", "itemId", "itemType") DO UPDATE SET "updatedAt" = NOW()
	`, caller.UserID, projectID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create project")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create project")
		return
	}

	writeOK(w, Project{
		ID:        projectID,
		Name:      req.Name,
		UserID:    Owner(caller.UserID),
		ParentID:  req.ProjectParentID,
		CreatedAt: timePtr(createdAt),
		UpdatedAt: timePtr(updatedAt),
	})
}

// getProject mirrors GET /projects/{id} — project metadata + caller access
// level.
func (s *Service) getProject(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	id := chiParam(r, "id")
	ctx := r.Context()
	level, err := s.accessLevelFor(ctx, caller, EntityTypeProject, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	if level < AccessLevelView {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// Full project row for timestamps.
	var p Project
	err = s.pool.QueryRow(ctx, `
		SELECT id, name, "userId", "parentId", "createdAt"::timestamptz,
		       "updatedAt"::timestamptz, "deletedAt"::timestamptz
		FROM "Project" WHERE id = $1
	`, id).Scan(&p.ID, &p.Name, &p.UserID, &p.ParentID, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get project")
		return
	}
	writeOK(w, map[string]any{
		"projectMetadata": p,
		"userAccessLevel": level.String(),
	})
}

// editProject mirrors PATCH /projects/{id} (PatchProjectRequestV2).
func (s *Service) editProject(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	id := chiParam(r, "id")
	var req struct {
		Name            *string          `json:"name"`
		ProjectParentID *string          `json:"projectParentId"`
		SharePermission *sharePermission `json:"sharePermission"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	level, err := s.accessLevelFor(ctx, caller, EntityTypeProject, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	if level < AccessLevelEdit {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if req.SharePermission != nil && level != AccessLevelOwner {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to edit project")
		return
	}
	defer tx.Rollback(ctx)

	if req.Name != nil || req.ProjectParentID != nil {
		q := `UPDATE "Project" SET "updatedAt" = NOW()`
		args := []any{id}
		if req.Name != nil {
			args = append(args, *req.Name)
			q += fmt.Sprintf(`, "name" = $%d`, len(args))
		}
		if req.ProjectParentID != nil {
			args = append(args, *req.ProjectParentID)
			q += fmt.Sprintf(`, "parentId" = $%d`, len(args))
		}
		q += ` WHERE id = $1`
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to edit project")
			return
		}
	}
	if req.SharePermission != nil {
		var spID string
		if err := tx.QueryRow(ctx,
			`SELECT "sharePermissionId" FROM "ProjectPermission" WHERE "projectId" = $1`,
			id).Scan(&spID); err == nil && spID != "" {
			var ls, lsl *string
			if req.SharePermission.LinkShare != nil {
				v := *req.SharePermission.LinkShare
				ls = &v
			}
			if req.SharePermission.LinkShareAccessLevel != nil {
				v := *req.SharePermission.LinkShareAccessLevel
				lsl = &v
			}
			if _, err := tx.Exec(ctx,
				`UPDATE "SharePermission" SET "linkShare" = COALESCE($2, "linkShare"),
				 "linkShareAccessLevel" = COALESCE($3, "linkShareAccessLevel"), "updatedAt" = NOW()
				 WHERE id = $1`, spID, ls, lsl); err != nil {
				writeErr(w, http.StatusInternalServerError, "unable to update share permission")
				return
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to edit project")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// deleteProject mirrors DELETE /projects/{id} — soft delete.
func (s *Service) deleteProject(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	id := chiParam(r, "id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeProject, id, AccessLevelOwner); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	tag, err := s.pool.Exec(ctx, `UPDATE "Project" SET "deletedAt" = NOW() WHERE id = $1 AND "deletedAt" IS NULL`, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete project")
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// revertDeleteProject mirrors PUT /projects/{id}/revert_delete.
func (s *Service) revertDeleteProject(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	id := chiParam(r, "id")
	ctx := r.Context()
	basic, err := s.q.GetBasicProject(ctx, id)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get project")
		return
	}
	if basic.UserID != caller.UserID && !caller.Internal {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, err := s.pool.Exec(ctx, `UPDATE "Project" SET "deletedAt" = NULL WHERE id = $1`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to revert delete")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// getProjectContent mirrors GET /projects/{id}/content — items directly in
// the project (documents, sub-projects, chats) with the caller's access
// level per item.
func (s *Service) getProjectContent(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	id := chiParam(r, "id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeProject, id, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}

	type item struct {
		ItemType    string     `json:"itemType"`
		ItemID      string     `json:"itemId"`
		Name        string     `json:"itemName"`
		Owner       string     `json:"owner"`
		FileType    *string    `json:"fileType,omitempty"`
		CreatedAt   *time.Time `json:"createdAt"`
		UpdatedAt   *time.Time `json:"updatedAt"`
		ViewedAt    *time.Time `json:"viewedAt,omitempty"`
		AccessLevel string     `json:"userAccessLevel"`
	}
	rows, err := s.pool.Query(ctx, `
		SELECT 'document' AS item_type, d.id, d.name, d.owner, d."fileType",
		       d."createdAt"::timestamptz, d."updatedAt"::timestamptz,
		       uh."updatedAt"::timestamptz AS viewed_at
		FROM "Document" d
		LEFT JOIN "UserHistory" uh ON uh."itemId" = d.id AND uh."userId" = $1 AND uh."itemType" = 'document'
		WHERE d."projectId" = $2 AND d."deletedAt" IS NULL
		UNION ALL
		SELECT 'project', p.id, p.name, p."userId", NULL,
		       p."createdAt"::timestamptz, p."updatedAt"::timestamptz,
		       uh."updatedAt"::timestamptz
		FROM "Project" p
		LEFT JOIN "UserHistory" uh ON uh."itemId" = p.id AND uh."userId" = $1 AND uh."itemType" = 'project'
		WHERE p."parentId" = $2 AND p."deletedAt" IS NULL
		UNION ALL
		SELECT 'chat', c.id, c.name, c."userId", NULL,
		       c."createdAt"::timestamptz, c."updatedAt"::timestamptz,
		       uh."updatedAt"::timestamptz
		FROM "Chat" c
		LEFT JOIN "UserHistory" uh ON uh."itemId" = c.id AND uh."userId" = $1 AND uh."itemType" = 'chat'
		WHERE c."projectId" = $2 AND c."deletedAt" IS NULL
		ORDER BY 7 DESC
	`, caller.UserID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get project content")
		return
	}
	defer rows.Close()
	items := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ItemType, &it.ItemID, &it.Name, &it.Owner,
			&it.FileType, &it.CreatedAt, &it.UpdatedAt, &it.ViewedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get project content")
			return
		}
		var lvl AccessLevel
		switch it.ItemType {
		case "document":
			lvl, _ = s.accessLevelFor(ctx, caller, EntityTypeDocument, it.ItemID)
		case "project":
			lvl, _ = s.accessLevelFor(ctx, caller, EntityTypeProject, it.ItemID)
		default:
			if it.Owner == caller.UserID {
				lvl = AccessLevelOwner
			} else {
				lvl = AccessLevelView
			}
		}
		it.AccessLevel = lvl.String()
		items = append(items, it)
	}
	writeOK(w, items)
}

// getProjectPermissions mirrors GET /projects/{id}/permissions.
func (s *Service) getProjectPermissions(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	id := chiParam(r, "id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeProject, id, AccessLevelOwner); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	var (
		spID         string
		linkShare    *string
		linkLevel    *string
		channelPerms []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT sp.id, sp."linkShare", sp."linkShareAccessLevel"::text,
		       COALESCE((SELECT json_agg(json_build_object('channel_id', c.channel_id, 'access_level', c.access_level))
		                 FROM "ChannelSharePermission" c WHERE c.share_permission_id = sp.id), '[]'::json)
		FROM "SharePermission" sp
		JOIN "ProjectPermission" pp ON pp."sharePermissionId" = sp.id
		WHERE pp."projectId" = $1
	`, id).Scan(&spID, &linkShare, &linkLevel, &channelPerms)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "share permission not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get permissions")
		return
	}
	resp := map[string]any{"id": spID}
	if linkShare != nil {
		resp["linkShare"] = *linkShare
	}
	if linkLevel != nil {
		resp["linkShareAccessLevel"] = *linkLevel
	}
	resp["channelSharePermissions"] = json.RawMessage(channelPerms)
	writeOK(w, map[string]any{"projectPermissions": resp})
}

// getProjectAccessLevel mirrors GET /projects/{id}/access_level.
func (s *Service) getProjectAccessLevel(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	id := chiParam(r, "id")
	level, err := s.accessLevelFor(r.Context(), caller, EntityTypeProject, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to get user access level")
		return
	}
	if level == AccessLevelNone {
		writeErr(w, http.StatusUnauthorized, "user does not have access to project")
		return
	}
	writeOK(w, map[string]any{"userAccessLevel": level.String()})
}

// getPendingProjects mirrors GET /projects/pending — projects with
// uploadPending. Basic port: list rows where the flag is set.
func (s *Service) getPendingProjects(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, name, "userId", "parentId", "createdAt"::timestamptz,
		       "updatedAt"::timestamptz, "deletedAt"::timestamptz
		FROM "Project"
		WHERE "userId" = $1 AND "uploadPending" = true
		ORDER BY "updatedAt" DESC
	`, caller.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get pending projects")
		return
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.UserID, &p.ParentID,
			&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get pending projects")
			return
		}
		projects = append(projects, p)
	}
	writeOK(w, projects)
}

// getBatchProjectPreview mirrors POST /projects/preview — tagged
// ProjectPreview enum per id.
func (s *Service) getBatchProjectPreview(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req struct {
		ProjectIDs []string `json:"project_ids"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	previews := make([]any, 0, len(req.ProjectIDs))
	for _, id := range req.ProjectIDs {
		basic, err := s.q.GetBasicProject(ctx, id)
		if err != nil {
			previews = append(previews, map[string]any{"access": "does_not_exist", "project_id": id})
			continue
		}
		level, err := s.accessLevelFor(ctx, caller, EntityTypeProject, id)
		if err != nil || level < AccessLevelView {
			previews = append(previews, map[string]any{"access": "no_access", "project_id": id})
			continue
		}
		// Path: ancestor chain names (Rust builds the full path).
		path := []string{basic.Name}
		parent := textPtr(basic.ParentID)
		for i := 0; i < 16 && parent != nil && *parent != ""; i++ {
			pp, err := s.q.GetBasicProject(ctx, *parent)
			if err != nil {
				break
			}
			path = append([]string{pp.Name}, path...)
			parent = textPtr(pp.ParentID)
		}
		previews = append(previews, map[string]any{
			"access":     "access",
			"id":         id,
			"name":       basic.Name,
			"owner":      basic.UserID,
			"path":       path,
			"updated_at": nil,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"previews": previews})
}
