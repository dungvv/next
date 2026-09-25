package dss

// misc.go ports the smaller dss surfaces: pins, history, recents,
// user_document_view_location, entity permission, item-id helpers,
// instructions, and saved views.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// ---------- pins ----------

// pinItem is the soup-ish item attached to a pin (Item is a tagged enum:
// {"type":"document"|"chat"|"project", ...fields}).
type pinItem map[string]any

// getPins mirrors GET /pins — {"error":false,"data":{"recent":[PinnedItem]}}.
func (s *Service) getPins(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	// GetPins2 joins entity_properties for task completion; the values param
	// is the "Completed" option id which is per-team — pass an empty value so
	// tasks report is_completed=false rather than erroring. TODO(port):
	// resolve the caller's Status property definition.
	rows, err := s.q.GetPins2(r.Context(), macrodb.GetPins2Params{
		UserId:               caller.UserID,
		Values:               nil,
		PropertyDefinitionID: pgtype.UUID{},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get pins")
		return
	}
	pins := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var it pinItem
		switch row.ItemType {
		case "document":
			var verID int64
			_, _ = fmtSscanInt(row.DocumentVersionID, &verID)
			it = pinItem{
				"type":                     "document",
				"id":                       row.ID,
				"document_version_id":      verID,
				"owner":                    row.UserID,
				"name":                     row.Name,
				"file_type":                textPtr(row.FileType),
				"sha":                      nilIfEmpty(row.Sha),
				"project_id":               textPtr(row.ProjectID),
				"branched_from_id":         textPtr(row.BranchedFromID),
				"branched_from_version_id": int8Ptr(row.BranchedFromVersionID),
				"document_family_id":       int8Ptr(row.DocumentFamilyID),
				"created_at":               timePtr(row.CreatedAt),
				"updated_at":               timePtr(row.UpdatedAt),
			}
		case "chat":
			it = pinItem{
				"type":          "chat",
				"id":            row.ID,
				"name":          row.Name,
				"user_id":       row.UserID,
				"project_id":    textPtr(row.ProjectID),
				"is_persistent": row.IsPersistent.Valid && row.IsPersistent.Bool,
				"created_at":    timePtr(row.CreatedAt),
				"updated_at":    timePtr(row.UpdatedAt),
			}
		case "project":
			it = pinItem{
				"type":       "project",
				"id":         row.ID,
				"name":       row.Name,
				"user_id":    row.UserID,
				"parent_id":  textPtr(row.ProjectID),
				"created_at": timePtr(row.CreatedAt),
				"updated_at": timePtr(row.UpdatedAt),
			}
		default:
			continue
		}
		pins = append(pins, map[string]any{
			"pin_index": row.PinIndex,
			"item":      it,
			"activity":  it,
		})
	}
	writeOK(w, map[string]any{"recent": pins})
}

func fmtSscanInt(s string, out *int64) (int, error) {
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + int64(c-'0')
	}
	*out = v
	return 1, nil
}

// addPin mirrors POST /pins/{pinned_item_id} — body {pinType, pinIndex}.
func (s *Service) addPin(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	itemID := chiParam(r, "pinned_item_id")
	var req struct {
		PinType  string `json:"pinType"`
		PinIndex int32  `json:"pinIndex"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.PinType == "" {
		writeErr(w, http.StatusBadRequest, "pinType is required")
		return
	}
	ctx := r.Context()
	// View access check on the pinned item (PinAccessLevelExtractor).
	et := EntityType(req.PinType)
	if et == EntityTypeDocument || et == EntityTypeProject {
		if _, err := s.requireAccess(ctx, caller, et, itemID, AccessLevelView); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "unable to check access")
			return
		}
	}
	if err := s.q.UpsertPin(ctx, macrodb.UpsertPinParams{
		UserId:         caller.UserID,
		PinnedItemId:   itemID,
		PinnedItemType: req.PinType,
		PinIndex:       req.PinIndex,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to add pin")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// removePin mirrors DELETE /pins/{pinned_item_id} — body {pinType}.
func (s *Service) removePin(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	itemID := chiParam(r, "pinned_item_id")
	var req struct {
		PinType string `json:"pinType"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if err := s.q.RemovePin(r.Context(), macrodb.RemovePinParams{
		UserId:         caller.UserID,
		PinnedItemId:   itemID,
		PinnedItemType: req.PinType,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to remove pin")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// reorderPins mirrors PATCH /pins — body is a bare array of
// {pinnedItemId, pinnedItemType, pinIndex}.
func (s *Service) reorderPins(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req []struct {
		PinnedItemID   string `json:"pinnedItemId"`
		PinnedItemType string `json:"pinnedItemType"`
		PinIndex       int32  `json:"pinIndex"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ids := make([]string, 0, len(req))
	types := make([]string, 0, len(req))
	indexes := make([]int32, 0, len(req))
	for _, p := range req {
		ids = append(ids, p.PinnedItemID)
		types = append(types, p.PinnedItemType)
		indexes = append(indexes, p.PinIndex)
	}
	if err := s.q.ReorderPins(r.Context(), macrodb.ReorderPinsParams{
		Column1: ids,
		Column2: types,
		Column3: indexes,
		UserId:  caller.UserID,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to reorder pins")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// ---------- history ----------

// getHistory mirrors GET /history — the caller's UserHistory items joined
// with their entities. Rust returns Vec<Item> (tagged enum).
func (s *Service) getHistory(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT uh."itemType", uh."itemId", uh."updatedAt"::timestamptz AS viewed_at,
		       COALESCE(d.name, c.name, p.name) AS name,
		       COALESCE(d.owner, c."userId", p."userId") AS owner,
		       d."fileType", COALESCE(d."createdAt"::timestamptz, c."createdAt"::timestamptz, p."createdAt"::timestamptz) AS created_at,
		       COALESCE(d."updatedAt"::timestamptz, c."updatedAt"::timestamptz, p."updatedAt"::timestamptz) AS updated_at,
		       d."projectId"
		FROM "UserHistory" uh
		LEFT JOIN "Document" d ON uh."itemType" = 'document' AND uh."itemId" = d.id AND d."deletedAt" IS NULL
		LEFT JOIN "Chat" c ON uh."itemType" = 'chat' AND uh."itemId" = c.id AND c."deletedAt" IS NULL
		LEFT JOIN "Project" p ON uh."itemType" = 'project' AND uh."itemId" = p.id AND p."deletedAt" IS NULL
		WHERE uh."userId" = $1
		ORDER BY uh."updatedAt" DESC
	`, caller.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get history")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var (
			itemType, itemID, name, owner  string
			viewedAt, createdAt, updatedAt *time.Time
			fileType, projectID            *string
		)
		if err := rows.Scan(&itemType, &itemID, &viewedAt, &name, &owner, &fileType, &createdAt, &updatedAt, &projectID); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get history")
			return
		}
		if name == "" {
			continue // deleted entity
		}
		it := map[string]any{
			"type":       itemType,
			"id":         itemID,
			"name":       name,
			"created_at": createdAt,
			"updated_at": updatedAt,
			"viewed_at":  viewedAt,
		}
		switch itemType {
		case "document":
			it["owner"] = owner
			it["file_type"] = fileType
			it["project_id"] = projectID
		case "chat":
			it["user_id"] = owner
			it["project_id"] = projectID
		case "project":
			it["user_id"] = owner
		}
		items = append(items, it)
	}
	writeOK(w, items)
}

// upsertHistory mirrors POST /history/{item_type}/{item_id} — updates
// ItemLastAccessed and the UserHistory row.
func (s *Service) upsertHistory(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	itemType := chiParam(r, "item_type")
	itemID := chiParam(r, "item_id")
	ctx := r.Context()

	// Access check for document/project items.
	var et EntityType
	switch itemType {
	case "document":
		et = EntityTypeDocument
	case "project":
		et = EntityTypeProject
	}
	if et != "" {
		if _, err := s.requireAccess(ctx, caller, et, itemID, AccessLevelView); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "unable to check access")
			return
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to begin transaction")
		return
	}
	defer tx.Rollback(ctx)
	if itemType != "thread" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO "ItemLastAccessed" (item_id, item_type, last_accessed)
			VALUES ($1, $2, NOW())
			ON CONFLICT (item_id, item_type) DO UPDATE SET last_accessed = NOW()
		`, itemID, itemType); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to update item last accessed")
			return
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "UserHistory" ("userId", "itemId", "itemType", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT ("userId", "itemId", "itemType") DO UPDATE SET "updatedAt" = NOW()
	`, caller.UserID, itemID, itemType); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to upsert history")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to upsert history")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// deleteHistory mirrors DELETE /history/{item_type}/{item_id}.
func (s *Service) deleteHistory(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
		DELETE FROM "UserHistory" WHERE "userId" = $1 AND "itemId" = $2 AND "itemType" = $3
	`, caller.UserID, chiParam(r, "item_id"), chiParam(r, "item_type")); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete history")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// ---------- recents ----------

// recentlyDeleted mirrors GET /recents/deleted — the caller's recently
// deleted items (documents + projects + chats), tagged Item enum.
func (s *Service) recentlyDeleted(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	rows, err := s.q.GetRecentlyDeleted(r.Context(), macrodb.GetRecentlyDeletedParams{
		Owner:                caller.UserID,
		Values:               nil,
		PropertyDefinitionID: pgtype.UUID{},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get recently deleted")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		it := map[string]any{
			"type":       row.ItemType,
			"id":         row.ID,
			"name":       row.Name,
			"created_at": timePtr(row.CreatedAt),
			"updated_at": timePtr(row.UpdatedAt),
			"deleted_at": timePtr(row.DeletedAt),
		}
		switch row.ItemType {
		case "document":
			var verID int64
			_, _ = fmtSscanInt(row.DocumentVersionID, &verID)
			it["document_version_id"] = verID
			it["owner"] = row.UserID
			it["file_type"] = textPtr(row.FileType)
			it["project_id"] = textPtr(row.ProjectID)
		case "chat":
			it["user_id"] = row.UserID
			it["is_persistent"] = row.IsPersistent.Valid && row.IsPersistent.Bool
			it["project_id"] = textPtr(row.ProjectID)
		case "project":
			it["user_id"] = row.UserID
			it["parent_id"] = textPtr(row.ProjectID)
		}
		items = append(items, it)
	}
	writeOK(w, items)
}

// ---------- user_document_view_location ----------

// getUserDocumentViewLocation mirrors GET /user_document_view_location/{document_id}.
func (s *Service) getUserDocumentViewLocation(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	loc, err := s.q.GetUserDocumentViewLocation(r.Context(), macrodb.GetUserDocumentViewLocationParams{
		UserID:     caller.UserID,
		DocumentID: chiParam(r, "document_id"),
	})
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "location not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get view location")
		return
	}
	writeOK(w, map[string]any{"location": loc.Location})
}

// upsertUserDocumentViewLocation mirrors POST /user_document_view_location/{document_id}.
func (s *Service) upsertUserDocumentViewLocation(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req struct {
		Location string `json:"location"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if err := s.q.UpsertUserDocumentViewLocation(r.Context(), macrodb.UpsertUserDocumentViewLocationParams{
		UserID:     caller.UserID,
		DocumentID: chiParam(r, "document_id"),
		Location:   req.Location,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to upsert view location")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// deleteUserDocumentViewLocation mirrors DELETE /user_document_view_location/{document_id}.
func (s *Service) deleteUserDocumentViewLocation(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	if err := s.q.DeleteUserDocumentViewLocation(r.Context(), macrodb.DeleteUserDocumentViewLocationParams{
		UserID:     caller.UserID,
		DocumentID: chiParam(r, "document_id"),
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete view location")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// ---------- entity permission ----------

// getEntityPermission mirrors GET /entity/{entity_type}/{entity_id}/permissions.
// Response: {"status":"access","permission":{"access_level":"..."}} or
// {"status":"no_access"}.
func (s *Service) getEntityPermission(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	entityType := chiParam(r, "entity_type")
	entityID := chiParam(r, "entity_id")

	var level AccessLevel
	var err error
	switch EntityType(entityType) {
	case EntityTypeDocument:
		level, err = s.accessLevelFor(r.Context(), caller, EntityTypeDocument, entityID)
	case EntityTypeProject:
		level, err = s.accessLevelFor(r.Context(), caller, EntityTypeProject, entityID)
	default:
		// Other entity kinds (chat, channel, thread, ...) are not ported;
		// owner-style rows only.
		level, err = AccessLevelNone, nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	if level < AccessLevelView {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "no_access"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "access",
		"permission": map[string]any{
			"access_level": level.String(),
		},
	})
}

// ---------- item ids ----------

// ShareableItem / UserAccessibleItem mirror
// model::document_storage_service_internal types (snake_case).
type shareableItem struct {
	ItemID   string `json:"item_id"`
	ItemType string `json:"item_type"`
}

// validateItemIDs mirrors POST /internal/validate_item_ids (also exposed
// under /items).
func (s *Service) validateItemIDs(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	if caller.UserID == "" || caller.UserID == internalUserID {
		writeErr(w, http.StatusUnauthorized, "No user id found in context")
		return
	}
	var req struct {
		Items []shareableItem `json:"items"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	var docIDs, chatIDs, projectIDs []string
	for _, it := range req.Items {
		switch it.ItemType {
		case "document":
			docIDs = append(docIDs, it.ItemID)
		case "chat":
			chatIDs = append(chatIDs, it.ItemID)
		case "project":
			projectIDs = append(projectIDs, it.ItemID)
		}
	}
	rows, err := s.q.ValidateUserAccessibleItems(r.Context(), macrodb.ValidateUserAccessibleItemsParams{
		UserID:  caller.UserID,
		Column2: docIDs,
		Column3: chatIDs,
		Column4: projectIDs,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get item ids")
		return
	}
	items := make([]shareableItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, shareableItem{ItemID: row.ItemID, ItemType: row.ItemType})
	}
	// Bare Json response in Rust.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getItemIDs mirrors GET /internal/item_ids?item_type=&exclude_owned=
// (also mounted under /items for convenience).
func (s *Service) getItemIDs(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	if caller.UserID == "" || caller.UserID == internalUserID {
		writeErr(w, http.StatusUnauthorized, "No user id found in context")
		return
	}
	itemType := r.URL.Query().Get("item_type")
	excludeOwned := r.URL.Query().Get("exclude_owned") == "true"
	ctx := r.Context()

	out := []shareableItem{}
	add := func(ids []string, t string, err error) bool {
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get item ids")
			return false
		}
		for _, id := range ids {
			out = append(out, shareableItem{ItemID: id, ItemType: t})
		}
		return true
	}
	if itemType == "" || itemType == "document" {
		ids, err := s.q.AccessibleDocuments(ctx, macrodb.AccessibleDocumentsParams{
			Owner:   caller.UserID,
			Column2: excludeOwned,
		})
		if !add(ids, "document", err) {
			return
		}
	}
	if itemType == "" || itemType == "chat" {
		ids, err := s.q.AccessibleChats(ctx, macrodb.AccessibleChatsParams{
			UserId:  caller.UserID,
			Column2: excludeOwned,
		})
		if !add(ids, "chat", err) {
			return
		}
	}
	if itemType == "" || itemType == "project" {
		ids, err := s.q.AccessibleProjects(ctx, macrodb.AccessibleProjectsParams{
			UserId:  caller.UserID,
			Column2: excludeOwned,
		})
		if !add(ids, "project", err) {
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ---------- instructions ----------

// getInstructions mirrors GET /instructions — {"document_id": "..."}.
func (s *Service) getInstructions(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID, err := s.q.GetInstructionsDocument(r.Context(), caller.UserID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "User does not have an instructions document")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to get instructions document")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"document_id": docID})
}

// createInstructions mirrors POST /instructions — creates the caller's
// instructions document (idempotent).
func (s *Service) createInstructions(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	if err := s.q.CreateInstructionsDocument(r.Context(), caller.UserID); err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to create instructions document")
		return
	}
	docID, err := s.q.GetInstructionsDocument(r.Context(), caller.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to get instructions document")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"document_id": docID})
}

// ---------- saved views ----------

// SavedView mirrors the saved_view table row (snake_case response fields
// per the Rust view model).
type savedView struct {
	ID        string          `json:"id"`
	UserID    string          `json:"user_id"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	CreatedAt *time.Time      `json:"created_at"`
	UpdatedAt *time.Time      `json:"updated_at"`
}

// getSavedViews mirrors GET /saved_views —
// {"views":[...],"excluded_default_views":[...]}.
func (s *Service) getSavedViews(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT id::text, user_id, name, config, created_at, updated_at
		FROM saved_view WHERE user_id = $1 ORDER BY created_at ASC
	`, caller.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get saved views")
		return
	}
	defer rows.Close()
	views := []savedView{}
	for rows.Next() {
		var v savedView
		if err := rows.Scan(&v.ID, &v.UserID, &v.Name, &v.Config, &v.CreatedAt, &v.UpdatedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get saved views")
			return
		}
		views = append(views, v)
	}
	exRows, err := s.pool.Query(r.Context(), `
		SELECT default_view_id FROM excluded_default_view WHERE user_id = $1
	`, caller.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get saved views")
		return
	}
	defer exRows.Close()
	excluded := []map[string]any{}
	for exRows.Next() {
		var id string
		if err := exRows.Scan(&id); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get saved views")
			return
		}
		excluded = append(excluded, map[string]any{"default_view_id": id})
	}
	// Bare Json response in Rust (ViewsResponse, camelCase envelope).
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"views":                views,
		"excludedDefaultViews": excluded,
	})
}

// createSavedView mirrors POST /saved_views — body {name, config}.
func (s *Service) createSavedView(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req struct {
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	var v savedView
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO saved_view (id, user_id, name, config)
		VALUES (gen_random_uuid(), $1, $2, $3)
		RETURNING id::text, user_id, name, config, created_at, updated_at
	`, caller.UserID, req.Name, []byte(req.Config)).
		Scan(&v.ID, &v.UserID, &v.Name, &v.Config, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create saved view")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// patchSavedView mirrors PATCH /saved_views/{saved_view_id} — {name?, config?}.
func (s *Service) patchSavedView(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	id := chiParam(r, "saved_view_id")
	var req struct {
		Name   *string          `json:"name"`
		Config *json.RawMessage `json:"config"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	var v savedView
	err := s.pool.QueryRow(r.Context(), `
		UPDATE saved_view SET
			name = COALESCE($3, name),
			config = COALESCE($4, config),
			updated_at = NOW()
		WHERE id = $1 AND user_id = $2
		RETURNING id::text, user_id, name, config, created_at, updated_at
	`, id, caller.UserID, req.Name, (*[]byte)(req.Config)).
		Scan(&v.ID, &v.UserID, &v.Name, &v.Config, &v.CreatedAt, &v.UpdatedAt)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "saved view not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to update saved view")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// deleteSavedView mirrors DELETE /saved_views/{saved_view_id}.
func (s *Service) deleteSavedView(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	tag, err := s.pool.Exec(r.Context(),
		`DELETE FROM saved_view WHERE id = $1 AND user_id = $2`,
		chiParam(r, "saved_view_id"), caller.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete saved view")
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "saved view not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// excludeDefaultSavedView mirrors POST /saved_views/exclude_default —
// {default_view_id}.
func (s *Service) excludeDefaultSavedView(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req struct {
		DefaultViewID string `json:"default_view_id"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
		INSERT INTO excluded_default_view (id, user_id, default_view_id)
		VALUES (gen_random_uuid(), $1, $2)
		ON CONFLICT DO NOTHING
	`, caller.UserID, req.DefaultViewID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to exclude default view")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// nilIfEmpty returns nil for empty strings (avoids serializing "").
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
