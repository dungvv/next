package dss

// soup.go ports a minimal but functional soup feed: the union of the
// caller's accessible documents, projects, and chats ordered by recency.
// The full Rust soup engine (filter AST, frecency scoring, email/channel/
// call/crm entity kinds, grouped bins) is a much deeper port — this provides
// the core document/project/chat page that the frontend and GraphQL adapter
// need, with TODOs marking the dropped filter semantics.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/dss/gql"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// soupRow is one row of the unified feed query.
type soupRow struct {
	itemType     string // document | project | chat
	id           string
	name         string
	owner        string
	fileType     *string
	projectID    *string
	subType      *string
	sha          *string
	versionID    int64
	isPersistent bool
	createdAt    *time.Time
	updatedAt    *time.Time
	viewedAt     *time.Time
	deletedAt    *time.Time
	accessLevel  string
}

// soupPageSQL unions the caller-visible documents, projects, and chats.
// "Accessible" = owner or an entity_access grant from the caller's source
// ids (user id, team ids, channel ids). Mirrors the access portion of the
// Rust soup query; filtering/frecency are TODO.
const soupPageSQL = `
WITH user_source_ids AS (
	SELECT cp.channel_id::text AS source_id FROM comms_channel_participants cp
		WHERE cp.user_id = $1 AND cp.left_at IS NULL
	UNION ALL
	SELECT t.team_id::text FROM team_user t WHERE t.user_id = $1
	UNION ALL
	SELECT $1
), accessible AS (
	SELECT ea.entity_id::text AS entity_id, ea.entity_type, MAX(ea.access_level::text) AS access_level
	FROM entity_access ea
	WHERE ea.source_id = ANY(SELECT source_id FROM user_source_ids)
	GROUP BY ea.entity_id, ea.entity_type
), combined AS (
	SELECT 'document' AS item_type, d.id, d.name, d.owner,
	       d."fileType", d."projectId", dt.sub_type::text AS sub_type,
	       di.sha, COALESCE(di.id, db.id) AS version_id, false AS is_persistent,
	       d."createdAt"::timestamptz AS created_at,
	       d."updatedAt"::timestamptz AS updated_at,
	       uh."updatedAt"::timestamptz AS viewed_at,
	       d."deletedAt"::timestamptz AS deleted_at,
	       CASE WHEN d.owner = $1 THEN 'owner' ELSE COALESCE(a.access_level, 'view') END AS access_level
	FROM "Document" d
	LEFT JOIN document_sub_type dt ON dt.document_id = d.id
	LEFT JOIN accessible a ON a.entity_type = 'document' AND a.entity_id = d.id
	LEFT JOIN "UserHistory" uh ON uh."itemId" = d.id AND uh."userId" = $1 AND uh."itemType" = 'document'
	LEFT JOIN LATERAL (
		SELECT i.id, i.sha FROM "DocumentInstance" i
		WHERE i."documentId" = d.id ORDER BY i."updatedAt" DESC LIMIT 1
	) di ON d."fileType" IS DISTINCT FROM 'docx'
	LEFT JOIN LATERAL (
		SELECT b.id FROM "DocumentBom" b
		WHERE b."documentId" = d.id ORDER BY b."createdAt" DESC LIMIT 1
	) db ON d."fileType" = 'docx'
	WHERE d."deletedAt" IS NULL
	  AND (d.owner = $1 OR a.entity_id IS NOT NULL)

	UNION ALL

	SELECT 'project', p.id, p.name, p."userId", NULL, p."parentId", NULL, NULL, 0, false,
	       p."createdAt"::timestamptz, p."updatedAt"::timestamptz,
	       uh."updatedAt"::timestamptz, p."deletedAt"::timestamptz,
	       CASE WHEN p."userId" = $1 THEN 'owner' ELSE COALESCE(a.access_level, 'view') END
	FROM "Project" p
	LEFT JOIN accessible a ON a.entity_type = 'project' AND a.entity_id = p.id
	LEFT JOIN "UserHistory" uh ON uh."itemId" = p.id AND uh."userId" = $1 AND uh."itemType" = 'project'
	WHERE p."deletedAt" IS NULL
	  AND (p."userId" = $1 OR a.entity_id IS NOT NULL)

	UNION ALL

	SELECT 'chat', c.id, c.name, c."userId", NULL, c."projectId", NULL, NULL, 0,
	       c."isPersistent",
	       c."createdAt"::timestamptz, c."updatedAt"::timestamptz,
	       uh."updatedAt"::timestamptz, c."deletedAt"::timestamptz,
	       CASE WHEN c."userId" = $1 THEN 'owner' ELSE COALESCE(a.access_level, 'view') END
	FROM "Chat" c
	LEFT JOIN accessible a ON a.entity_type = 'chat' AND a.entity_id = c.id
	LEFT JOIN "UserHistory" uh ON uh."itemId" = c.id AND uh."userId" = $1 AND uh."itemType" = 'chat'
	WHERE c."deletedAt" IS NULL
	  AND (c."userId" = $1 OR a.entity_id IS NOT NULL)
)
SELECT item_type, id, name, owner, "fileType", "projectId", sub_type, sha,
       version_id, is_persistent, created_at, updated_at, viewed_at, deleted_at,
       access_level
FROM combined
`

// soupPage runs the unified feed query with keyset-free offset pagination.
// sortMethod: viewed_at | updated_at | created_at | viewed_updated.
func (s *Service) soupPage(ctx context.Context, userID string, limit, offset int, sortMethod string, asc bool) ([]soupRow, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	orderCol := `COALESCE(viewed_at, updated_at)`
	switch sortMethod {
	case "created_at", "CREATED_AT":
		orderCol = "created_at"
	case "updated_at", "UPDATED_AT":
		orderCol = "updated_at"
	case "viewed_at", "VIEWED_AT":
		orderCol = `COALESCE(viewed_at, 'epoch'::timestamptz)`
	}
	dir := "DESC"
	if asc {
		dir = "ASC"
	}
	q := soupPageSQL + fmt.Sprintf(" ORDER BY %s %s NULLS LAST, id %s LIMIT %d OFFSET %d",
		orderCol, dir, dir, limit+1, offset)
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []soupRow{}
	for rows.Next() {
		var r soupRow
		if err := rows.Scan(&r.itemType, &r.id, &r.name, &r.owner, &r.fileType,
			&r.projectID, &r.subType, &r.sha, &r.versionID, &r.isPersistent,
			&r.createdAt, &r.updatedAt, &r.viewedAt, &r.deletedAt, &r.accessLevel); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// soupCursor encodes the next offset as an opaque base64 token.
func encodeSoupCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset)))
}

func decodeSoupCursor(c string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, err
	}
	s := string(raw)
	if !strings.HasPrefix(s, "o:") {
		return 0, fmt.Errorf("bad cursor")
	}
	return strconv.Atoi(s[2:])
}

// soupAPIItem serializes one row as the Rust SoupApiItem shape:
// {"tag":"document","data":{...camelCase...,"properties":[]},
//
//	"frecency_score":0,"is_favorited":false}
func soupAPIItem(r soupRow) map[string]any {
	var data map[string]any
	switch r.itemType {
	case "document":
		data = map[string]any{
			"id":                r.id,
			"documentVersionId": r.versionID,
			"ownerId":           r.owner,
			"name":              r.name,
			"createdAt":         r.createdAt,
			"updatedAt":         r.updatedAt,
		}
		if r.fileType != nil {
			data["fileType"] = *r.fileType
		}
		if r.sha != nil {
			data["sha"] = *r.sha
		}
		if r.projectID != nil {
			data["projectId"] = *r.projectID
		}
		if r.viewedAt != nil {
			data["viewedAt"] = *r.viewedAt
		}
		if r.deletedAt != nil {
			data["deletedAt"] = *r.deletedAt
		}
		if r.subType != nil {
			// SoupDocumentSubType is internally tagged {"type":"task",...}
			data["subType"] = map[string]any{"type": *r.subType}
		}
		data["properties"] = []any{}
	case "project":
		data = map[string]any{
			"id":        r.id,
			"name":      r.name,
			"ownerId":   r.owner,
			"createdAt": r.createdAt,
			"updatedAt": r.updatedAt,
		}
		if r.projectID != nil {
			data["parentId"] = *r.projectID
		}
		if r.viewedAt != nil {
			data["viewedAt"] = *r.viewedAt
		}
		data["properties"] = []any{}
	case "chat":
		data = map[string]any{
			"id":           r.id,
			"name":         r.name,
			"ownerId":      r.owner,
			"isPersistent": r.isPersistent,
			"createdAt":    r.createdAt,
			"updatedAt":    r.updatedAt,
		}
		if r.projectID != nil {
			data["projectId"] = *r.projectID
		}
		if r.viewedAt != nil {
			data["viewedAt"] = *r.viewedAt
		}
		data["properties"] = []any{}
	default:
		return nil
	}
	return map[string]any{
		"tag":            r.itemType,
		"data":           data,
		"frecency_score": 0.0,
		"is_favorited":   false,
	}
}

// soupParams extracts limit/cursor/sort from the soup request params.
// The Rust Params struct carries limit/sortMethod/etc. inside the POST body
// (or query params for GET). Unknown filter fields are ignored — the
// no-filter page is returned. TODO(port): filter AST evaluation.
type soupParams struct {
	Limit      int
	Offset     int
	SortMethod string
	Asc        bool
}

func (s *Service) soupParamsFromRequest(r *http.Request, bodyParams map[string]any) (soupParams, error) {
	p := soupParams{Limit: 20}
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		off, err := decodeSoupCursor(v)
		if err != nil {
			return p, fmt.Errorf("invalid cursor")
		}
		p.Offset = off
	}
	if v := q.Get("sort_method"); v != "" {
		p.SortMethod = v
	}
	p.Asc = q.Get("sort_direction") == "asc" || q.Get("sortDirection") == "ASC"

	if bodyParams != nil {
		if v, ok := bodyParams["limit"].(float64); ok {
			p.Limit = int(v)
		}
		if v, ok := bodyParams["sortMethod"].(string); ok {
			p.SortMethod = v
		}
		if v, ok := bodyParams["sortDirection"].(string); ok {
			p.Asc = strings.EqualFold(v, "asc")
		}
	}
	return p, nil
}

func (s *Service) serveSoupPage(w http.ResponseWriter, r *http.Request, p soupParams) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	rows, hasMore, err := s.soupPage(r.Context(), caller.UserID, p.Limit, p.Offset, p.SortMethod, p.Asc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get soup")
		return
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		if it := soupAPIItem(row); it != nil {
			items = append(items, it)
		}
	}
	var nextCursor any
	if hasMore {
		nextCursor = encodeSoupCursor(p.Offset + p.Limit)
	}
	// PaginatedOpaqueCursor serializes {"items":[...],"next_cursor":...}.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

// getSoup mirrors GET /items/soup.
func (s *Service) getSoup(w http.ResponseWriter, r *http.Request) {
	p, err := s.soupParamsFromRequest(r, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.serveSoupPage(w, r, p)
}

// postSoup mirrors POST /items/soup — filters are accepted but only the
// pagination/sort params are honored in this port. TODO(port): EntityFilters.
func (s *Service) postSoup(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	var params map[string]any
	if v, ok := body["params"].(map[string]any); ok {
		params = v
	}
	p, err := s.soupParamsFromRequest(r, params)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.serveSoupPage(w, r, p)
}

// postSoupAST mirrors POST /items/soup/ast — same as postSoup with the AST
// filter shape flattened into the body. TODO(port): ApiEntityFilterAst.
func (s *Service) postSoupAST(w http.ResponseWriter, r *http.Request) {
	s.postSoup(w, r)
}

// ---------- gql.Backend implementation ----------

// ViewerUserID implements gql.Backend.
func (s *Service) ViewerUserID(ctx context.Context) string {
	caller, ok := callerFromCtx(ctx)
	if !ok || caller.Internal && caller.UserID == internalUserID {
		return ""
	}
	return caller.UserID
}

// callerFromCtx adapts auth.FromContext for the gql backend.
func callerFromCtx(ctx context.Context) (Caller, bool) {
	return auth.FromContext(ctx)
}

// SoupPage implements gql.Backend — maps soup rows to GraphqlSoupEntity.
func (s *Service) SoupPage(ctx context.Context, input gql.SoupInput) (*gql.SoupPageResult, error) {
	userID := s.ViewerUserID(ctx)
	if userID == "" {
		return nil, fmt.Errorf("unauthenticated")
	}
	limit := 20
	offset := 0
	sort := "VIEWED_UPDATED"
	asc := false
	if input.Initial != nil {
		if input.Initial.Limit != nil {
			limit = *input.Initial.Limit
		}
		if input.Initial.SortMethod != nil {
			sort = string(*input.Initial.SortMethod)
		}
		if input.Initial.SortDirection != nil {
			asc = *input.Initial.SortDirection == gql.GraphqlSortDirectionAsc
		}
		// TODO(port): input.Initial.Filters (GraphqlEntityFilterAst) is not
		// evaluated — the unfiltered accessible page is returned.
	}
	if input.Continuation != nil {
		off, err := decodeSoupCursor(input.Continuation.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor")
		}
		offset = off
		if input.Continuation.SortDirection != nil {
			asc = *input.Continuation.SortDirection == gql.GraphqlSortDirectionAsc
		}
	}
	rows, hasMore, err := s.soupPage(ctx, userID, limit, offset, sort, asc)
	if err != nil {
		return nil, err
	}
	items := make([]gql.GraphqlSoupEntity, 0, len(rows))
	for _, row := range rows {
		if e := soupGraphqlEntity(row); e != nil {
			items = append(items, e)
		}
	}
	var next *string
	if hasMore {
		c := encodeSoupCursor(offset + limit)
		next = &c
	}
	return &gql.SoupPageResult{Items: items, NextCursor: next}, nil
}

// rfc3339 renders an optional timestamp in RFC 3339 form.
func rfc3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// soupGraphqlEntity maps a soup row to the concrete GraphqlSoupEntity model.
func soupGraphqlEntity(r soupRow) gql.GraphqlSoupEntity {
	ownerType := gql.GraphqlOwnerTypeUser
	if strings.HasPrefix(r.owner, "bot|") {
		ownerType = gql.GraphqlOwnerTypeBot
	}
	meta := &gql.GraphqlEntityMetadata{
		OwnerID:   &r.owner,
		OwnerType: &ownerType,
		CreatedAt: rfc3339(r.createdAt),
		UpdatedAt: rfc3339(r.updatedAt),
		ViewedAt:  rfc3339(r.viewedAt),
		DeletedAt: rfc3339(r.deletedAt),
	}
	if r.projectID != nil {
		parentType := gql.GraphqlEntityTypeProject
		if r.itemType == "project" {
			meta.Parent = &gql.GraphqlEntity{ID: *r.projectID, EntityType: parentType}
		} else {
			meta.Parent = &gql.GraphqlEntity{ID: *r.projectID, EntityType: parentType}
		}
	}
	perm := gql.GraphqlAccessLevelPermission{
		AccessLevel: gqlAccessLevel(r.accessLevel),
	}
	switch r.itemType {
	case "document":
		name := r.name
		doc := gql.GraphqlSoupDocument{
			ID:               r.id,
			EntityType:       gql.GraphqlSoupEntityTypeDocument,
			DisplayName:      &name,
			Metadata:         meta,
			Name:             r.name,
			OwnerID:          r.owner,
			OwnerType:        ownerType,
			FileType:         r.fileType,
			ProjectID:        r.projectID,
			CreatedAt:        strOrEmpty(rfc3339(r.createdAt)),
			UpdatedAt:        strOrEmpty(rfc3339(r.updatedAt)),
			ViewedAt:         rfc3339(r.viewedAt),
			DeletedAt:        rfc3339(r.deletedAt),
			ViewerPermission: perm,
		}
		if r.subType != nil {
			switch *r.subType {
			case "task":
				doc.SubType = gql.GraphqlTaskSubType{IsCompleted: false}
			case "snippet":
				doc.SubType = gql.GraphqlSnippetSubType{}
			case "skill":
				doc.SubType = gql.GraphqlSkillSubType{}
			case "initiative_description":
				doc.SubType = gql.GraphqlInitiativeDescriptionSubType{}
			}
		}
		return doc
	case "project":
		name := r.name
		return gql.GraphqlSoupProject{
			ID:               r.id,
			EntityType:       gql.GraphqlSoupEntityTypeProject,
			DisplayName:      &name,
			Metadata:         meta,
			Name:             r.name,
			OwnerID:          r.owner,
			OwnerType:        ownerType,
			ParentID:         r.projectID,
			CreatedAt:        strOrEmpty(rfc3339(r.createdAt)),
			UpdatedAt:        strOrEmpty(rfc3339(r.updatedAt)),
			ViewedAt:         rfc3339(r.viewedAt),
			DeletedAt:        rfc3339(r.deletedAt),
			ViewerPermission: perm,
		}
	case "chat":
		name := r.name
		return gql.GraphqlSoupChat{
			ID:               r.id,
			EntityType:       gql.GraphqlSoupEntityTypeChat,
			DisplayName:      &name,
			Metadata:         meta,
			Name:             r.name,
			OwnerID:          r.owner,
			OwnerType:        ownerType,
			ProjectID:        r.projectID,
			IsPersistent:     r.isPersistent,
			CreatedAt:        strOrEmpty(rfc3339(r.createdAt)),
			UpdatedAt:        strOrEmpty(rfc3339(r.updatedAt)),
			ViewedAt:         rfc3339(r.viewedAt),
			DeletedAt:        rfc3339(r.deletedAt),
			ViewerPermission: perm,
		}
	}
	return nil
}

func gqlAccessLevel(s string) gql.GraphqlEntityAccessLevel {
	switch parseAccessLevel(s) {
	case AccessLevelOwner:
		return gql.GraphqlEntityAccessLevelOwner
	case AccessLevelEdit:
		return gql.GraphqlEntityAccessLevelEdit
	case AccessLevelComment:
		return gql.GraphqlEntityAccessLevelComment
	default:
		return gql.GraphqlEntityAccessLevelView
	}
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// EntityByID implements gql.Backend — loads one entity for the viewer.
func (s *Service) EntityByID(ctx context.Context, entityType gql.GraphqlSoupEntityType, id string) (gql.GraphqlSoupEntity, error) {
	userID := s.ViewerUserID(ctx)
	if userID == "" {
		return nil, fmt.Errorf("unauthenticated")
	}
	var et EntityType
	switch entityType {
	case gql.GraphqlSoupEntityTypeDocument:
		et = EntityTypeDocument
	case gql.GraphqlSoupEntityTypeProject:
		et = EntityTypeProject
	default:
		return nil, fmt.Errorf("entity type %s not supported", entityType)
	}
	caller, _ := callerFromCtx(ctx)
	level, err := s.accessLevelFor(ctx, caller, et, id)
	if err != nil {
		return nil, err
	}
	if level < AccessLevelView {
		return nil, fmt.Errorf("no access")
	}
	// Load the row through the soup page machinery by id — simpler to query
	// directly here.
	row, err := s.soupRowByID(ctx, userID, et, id)
	if err != nil || row == nil {
		return nil, fmt.Errorf("entity not found")
	}
	return soupGraphqlEntity(*row), nil
}

// soupRowByID loads a single document/project row in the soup shape.
func (s *Service) soupRowByID(ctx context.Context, userID string, et EntityType, id string) (*soupRow, error) {
	var q string
	switch et {
	case EntityTypeDocument:
		q = `
			SELECT 'document', d.id, d.name, d.owner, d."fileType", d."projectId",
			       dt.sub_type::text, di.sha, COALESCE(di.id, db.id), false,
			       d."createdAt"::timestamptz, d."updatedAt"::timestamptz,
			       uh."updatedAt"::timestamptz, d."deletedAt"::timestamptz,
			       CASE WHEN d.owner = $2 THEN 'owner' ELSE 'view' END
			FROM "Document" d
			LEFT JOIN document_sub_type dt ON dt.document_id = d.id
			LEFT JOIN "UserHistory" uh ON uh."itemId" = d.id AND uh."userId" = $2 AND uh."itemType" = 'document'
			LEFT JOIN LATERAL (SELECT i.id, i.sha FROM "DocumentInstance" i
				WHERE i."documentId" = d.id ORDER BY i."updatedAt" DESC LIMIT 1) di
				ON d."fileType" IS DISTINCT FROM 'docx'
			LEFT JOIN LATERAL (SELECT b.id FROM "DocumentBom" b
				WHERE b."documentId" = d.id ORDER BY b."createdAt" DESC LIMIT 1) db
				ON d."fileType" = 'docx'
			WHERE d.id = $1`
	case EntityTypeProject:
		q = `
			SELECT 'project', p.id, p.name, p."userId", NULL, p."parentId", NULL,
			       NULL, 0, false, p."createdAt"::timestamptz, p."updatedAt"::timestamptz,
			       uh."updatedAt"::timestamptz, p."deletedAt"::timestamptz,
			       CASE WHEN p."userId" = $2 THEN 'owner' ELSE 'view' END
			FROM "Project" p
			LEFT JOIN "UserHistory" uh ON uh."itemId" = p.id AND uh."userId" = $2 AND uh."itemType" = 'project'
			WHERE p.id = $1`
	default:
		return nil, fmt.Errorf("unsupported entity type")
	}
	var r soupRow
	err := s.pool.QueryRow(ctx, q, id, userID).Scan(&r.itemType, &r.id, &r.name,
		&r.owner, &r.fileType, &r.projectID, &r.subType, &r.sha, &r.versionID,
		&r.isPersistent, &r.createdAt, &r.updatedAt, &r.viewedAt, &r.deletedAt,
		&r.accessLevel)
	if err != nil {
		return nil, err
	}
	return &r, nil
}
