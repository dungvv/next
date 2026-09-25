package dss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// maxDocumentNameRunes mirrors MAX_DOCUMENT_NAME_GRAPHEMES (approximated with
// rune count — grapheme clusters need an external dep).
const maxDocumentNameRunes = 200

// presignTTL mirrors the Rust presigned-url expiry (env-configured there; a
// sane default here).
const presignTTL = time.Hour

// ---------- file-type helpers (subset of model_file_type::FileType) ----------

// staticFileTypes mirror FileType::is_static (pdf + images).
var staticFileTypes = map[string]bool{
	"pdf": true, "png": true, "jpg": true, "jpeg": true,
	"gif": true, "svg": true, "webp": true,
}

// mimeTypes mirrors FileType::mime_type for the common types.
var mimeTypes = map[string]string{
	"docx":        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"pdf":         "application/pdf",
	"md":          "text/markdown",
	"spreadsheet": "application/x-macro-spreadsheet",
	"canvas":      "application/x-macro-canvas",
	"png":         "image/png",
	"jpg":         "image/jpeg",
	"jpeg":        "image/jpeg",
	"gif":         "image/gif",
	"svg":         "image/svg+xml",
	"webp":        "image/webp",
	"txt":         "text/plain",
	"html":        "text/html",
	"json":        "application/json",
	"csv":         "text/csv",
	"zip":         "application/zip",
	"mp3":         "audio/mpeg",
	"mp4":         "video/mp4",
	"wav":         "audio/wav",
}

func mimeTypeFor(fileType string) string {
	if m, ok := mimeTypes[fileType]; ok {
		return m
	}
	return "text/plain" // most unknown file types map to text/plain in Rust
}

// splitSuffixMatch mirrors FileType::split_suffix_match — splits "name.ext"
// into ("name", "ext") when ext is a known file type. We approximate the
// Rust suffix trie with the mimeTypes/static sets plus a small extras list.
var knownExtensions = func() map[string]bool {
	m := map[string]bool{}
	for k := range mimeTypes {
		m[k] = true
	}
	for k := range staticFileTypes {
		m[k] = true
	}
	for _, e := range []string{
		"c", "h", "cpp", "cc", "cxx", "hpp", "hh", "hxx", "cs", "go", "rs",
		"py", "js", "jsx", "ts", "tsx", "java", "rb", "rs", "sh", "bash",
		"zsh", "fish", "sql", "yaml", "yml", "toml", "xml", "dockerfile",
		"makefile", "r", "lua", "swift", "kt", "kts", "scala", "clj", "ex",
		"exs", "erl", "hrl", "hs", "ml", "fs", "vb", "php", "pl", "pm",
		"dart", "vue", "svelte", "astro", "scss", "sass", "less", "css",
		"woff", "woff2", "ttf", "otf", "eot", "wasm", "dmg", "iso", "tar",
		"gz", "bz2", "xz", "7z", "rar", "doc", "xls", "xlsx", "ppt", "pptx",
		"odt", "ods", "odp", "epub", "mobi", "azw3", "heic", "heif", "tiff",
		"bmp", "ico", "avif", "webm", "mov", "avi", "mkv", "flac", "ogg",
	} {
		m[e] = true
	}
	return m
}()

// cleanDocumentName strips a known file extension from the name.
func cleanDocumentName(name string) (string, *string) {
	idx := strings.LastIndex(name, ".")
	if idx <= 0 || idx == len(name)-1 {
		return name, nil
	}
	ext := strings.ToLower(name[idx+1:])
	if !knownExtensions[ext] {
		return name, nil
	}
	return name[:idx], &ext
}

// ---------- middleware ----------

// ensureDocumentExists mirrors ensure_document_exists: loads the basic
// document, 404s when absent.
func (s *Service) ensureDocumentExists(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "document_id")
		_, err := s.q.GetBasicDocument(r.Context(), id)
		if isNoRows(err) {
			writeErr(w, http.StatusNotFound,
				fmt.Sprintf("document with id %q was not found", id))
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "unknown error occurred")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- handlers ----------

// getUserDocuments mirrors GET /documents (get_user_documents_handler).
func (s *Service) getUserDocuments(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	limit := queryInt(r, "limit", 10)
	offset := queryInt(r, "offset", 0)
	if limit > 100 {
		writeErr(w, http.StatusBadRequest, "limit must be less than or equal to 100")
		return
	}
	fileType := r.URL.Query().Get("file_type")

	ctx := r.Context()
	args := []any{caller.UserID, limit, offset}
	countQuery := `SELECT COUNT(*) FROM "Document" d WHERE owner = $1 AND d."deletedAt" IS NULL`
	listQuery := docMetadataSelect + ` WHERE d.owner = $1 AND d."deletedAt" IS NULL`
	if fileType != "" {
		countQuery += ` AND d."fileType" = $2`
		listQuery += ` AND d."fileType" = $4`
		args = append(args, fileType)
	}
	listQuery += ` ORDER BY d."updatedAt" DESC LIMIT $2 OFFSET $3`

	var total int64
	if err := s.pool.QueryRow(ctx, countQuery, countArgs(caller.UserID, fileType)...).Scan(&total); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to get documents")
		return
	}
	resp := UserDocumentsResponse{Documents: []DocumentMetadata{}, Total: total}
	if total == 0 {
		writeOK(w, resp)
		return
	}
	rows, err := s.pool.Query(ctx, listQuery, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to get documents")
		return
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanDocumentMetadata(rows)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to get documents")
			return
		}
		resp.Documents = append(resp.Documents, m)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to get documents")
		return
	}
	if offset+limit < total {
		next := offset + limit
		resp.NextOffset = &next
	}
	writeOK(w, resp)
}

func countArgs(userID, fileType string) []any {
	if fileType == "" {
		return []any{userID}
	}
	return []any{userID, fileType}
}

// docMetadataSelect is the shared full-metadata SELECT used by the
// user-documents list (same shape as sqlc getDocument2).
const docMetadataSelect = `
	SELECT
		d.id as document_id,
		d.owner as owner,
		d.name as document_name,
		COALESCE(db.id, di.id) as document_version_id,
		d."branchedFromId" as branched_from_id,
		d."branchedFromVersionId" as branched_from_version_id,
		d."documentFamilyId" as document_family_id,
		d."fileType" as file_type,
		d."createdAt"::timestamptz as created_at,
		d."updatedAt"::timestamptz as updated_at,
		db.bom_parts as document_bom,
		di.modification_data as modification_data,
		d."projectId" as project_id,
		p.name as project_name,
		di.sha as sha,
		dt.sub_type as sub_type,
		d."deletedAt"::timestamptz as deleted_at
	FROM "Document" d
	LEFT JOIN document_sub_type dt ON dt.document_id = d.id
	LEFT JOIN LATERAL (
		SELECT b.id, (
			SELECT json_agg(json_build_object('id', bp.id, 'sha', bp.sha, 'path', bp.path))
			FROM "BomPart" bp WHERE bp."documentBomId" = b.id
		) as bom_parts
		FROM "DocumentBom" b WHERE b."documentId" = d.id
		ORDER BY b."createdAt" DESC LIMIT 1
	) db ON d."fileType" = 'docx'
	LEFT JOIN LATERAL (
		SELECT i.id, i.sha, i."createdAt", (
			SELECT imod."modificationData"
			FROM "DocumentInstanceModificationData" imod
			WHERE imod."documentInstanceId" = i.id
		) as modification_data, i."updatedAt"
		FROM "DocumentInstance" i WHERE i."documentId" = d.id
		ORDER BY i."updatedAt" DESC LIMIT 1
	) di ON d."fileType" IS DISTINCT FROM 'docx'
	LEFT JOIN LATERAL (
		SELECT p.name FROM "Project" p WHERE p.id = d."projectId"
	) p ON d."projectId" IS NOT NULL
`

func scanDocumentMetadata(rows pgx.Rows) (DocumentMetadata, error) {
	var (
		m        DocumentMetadata
		fileType *string
		bfID     *string
		bfVID    *int64
		famID    *int64
		projID   *string
		projName *string
		sha      *string
		subType  *string
	)
	err := rows.Scan(
		&m.DocumentID, &m.Owner, &m.DocumentName, &m.DocumentVersionID,
		&bfID, &bfVID, &famID, &fileType, &m.CreatedAt, &m.UpdatedAt,
		&m.DocumentBom, &m.ModificationData, &projID, &projName, &sha,
		&subType, &m.DeletedAt,
	)
	if err != nil {
		return m, err
	}
	m.FileType = fileType
	m.BranchedFromID = bfID
	m.BranchedFromVersionID = bfVID
	m.DocumentFamilyID = famID
	m.ProjectID = projID
	m.ProjectName = projName
	m.Sha = sha
	m.SubType = subType
	return m, nil
}

// getDocument mirrors GET /documents/{document_id} (documents hex get_document).
func (s *Service) getDocument(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()

	level, err := s.accessLevelFor(ctx, caller, EntityTypeDocument, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to check access")
		return
	}
	if level < AccessLevelView {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	row, err := s.q.GetDocument2(ctx, docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("document with id %q was not found", docID))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}

	var viewLocation *string
	if !caller.Internal {
		loc, err := s.q.GetUserDocumentViewLocation(ctx, macrodb.GetUserDocumentViewLocationParams{
			UserID:     caller.UserID,
			DocumentID: docID,
		})
		if err == nil && loc.Location != "" {
			viewLocation = &loc.Location
		} else if err != nil && !isNoRows(err) {
			writeErr(w, http.StatusInternalServerError, "error getting view location")
			return
		}
	}

	httpx.WriteJSON(w, http.StatusOK, okData(GetDocumentResponseData{
		DocumentMetadata: mapDocumentMetadata(row),
		UserAccessLevel:  level.String(),
		ViewLocation:     viewLocation,
	}))
}

// createDocument mirrors POST /documents (documents hex create_document).
func (s *Service) createDocument(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req CreateDocumentRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.EmailAttachmentID != nil && !caller.Internal {
		httpx.ErrorJSON(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if utf8.RuneCountInString(req.DocumentName) > maxDocumentNameRunes {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"code":      "DOCUMENT_NAME_TOO_LONG",
			"maxLength": maxDocumentNameRunes,
			"message":   "name too long",
		})
		return
	}
	// Project move requires edit access on the target project.
	if req.ProjectID != nil && *req.ProjectID != "" {
		if _, err := s.requireAccess(r.Context(), caller, EntityTypeProject, *req.ProjectID, AccessLevelEdit); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				httpx.ErrorJSON(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check project access")
			return
		}
	}

	// Resolve file type: explicit field wins; otherwise split the name suffix.
	fileType := req.FileType
	documentName := req.DocumentName
	if fileType != nil {
		if cleaned, _ := cleanDocumentName(documentName); cleaned != documentName {
			documentName = cleaned
		}
	} else if cleaned, ext := cleanDocumentName(documentName); ext != nil {
		documentName = cleaned
		fileType = ext
	}

	ctx := r.Context()
	created, err := s.createDocumentTx(ctx, caller.UserID, req, documentName, fileType)
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			httpx.ErrorJSON(w, http.StatusConflict, "document with ID already exists")
			return
		}
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to create document")
		return
	}

	// Track upload job linkage (Rust updates UploadJob.documentId).
	if req.JobID != nil && *req.JobID != "" {
		_, _ = s.pool.Exec(ctx, `UPDATE "UploadJob" SET "documentId" = $1 WHERE "jobId" = $2`,
			created.documentID, *req.JobID)
	}

	// Presigned PUT URL: {owner}/{doc_id}/{version_id}.
	contentType := "application/octet-stream"
	if fileType != nil {
		contentType = mimeTypeFor(*fileType)
	}
	var presigned *string
	key := fmt.Sprintf("%s/%s/%d", caller.UserID, created.documentID, created.versionID)
	if url, err := s.store.PresignPut(ctx, s.bucket, key, contentType, presignTTL); err == nil {
		presigned = &url
	} else {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to generate presigned url")
		return
	}

	// Publish document.created.
	var createdAt *string
	if created.createdAt != nil {
		s := created.createdAt.UTC().Format(time.RFC3339Nano)
		createdAt = &s
	}
	s.publishDocumentEvent(created.documentID, DocEventCreated, DocumentCreatedMetadata{
		DocumentID:   created.documentID,
		Owner:        caller.UserID,
		DocumentName: documentName,
		FileType:     fileType,
		ProjectID:    req.ProjectID,
		SubType:      created.subType,
		CreatedAt:    createdAt,
	})

	httpx.WriteJSON(w, http.StatusOK, okData(CreateDocumentResponseData{
		DocumentMetadata: DocumentResponseMetadata{
			DocumentID:        created.documentID,
			DocumentVersionID: created.versionID,
			Owner:             Owner(caller.UserID),
			DocumentName:      documentName,
			FileType:          fileType,
			Sha:               strPtr(req.Sha),
			CreatedAt:         created.createdAt,
			UpdatedAt:         created.createdAt,
			SubType:           created.subType,
		},
		PresignedURL: presigned,
		ContentType:  contentType,
		FileType:     fileType,
	}))
}

type createdDocument struct {
	documentID string
	versionID  int64
	createdAt  *time.Time
	subType    *string
}

// createDocumentTx ports create::insert_new_document: Document row, sub_type,
// version (DocumentBom for docx, DocumentInstance otherwise), share
// permission, history, entity_access owner grant, entity registry row.
func (s *Service) createDocumentTx(ctx context.Context, userID string, req CreateDocumentRequest, name string, fileType *string) (createdDocument, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return createdDocument{}, err
	}
	defer tx.Rollback(ctx)

	createdAt := time.Now()
	if req.CreatedAt != nil {
		createdAt = *req.CreatedAt
	}

	// Resolve document id.
	docID := uuid.Must(uuid.NewV7()).String()
	if req.ID != nil && *req.ID != "" {
		docID = *req.ID
	}

	var projectID pgtype.Text
	if req.ProjectID != nil {
		projectID = pgTextStr(*req.ProjectID)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO "Document" (id, owner, name, "fileType", "projectId", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, docID, userID, name, pgText(fileType), projectID, pgtype.Timestamp{Time: createdAt, Valid: true}); err != nil {
		return createdDocument{}, err
	}

	// Sub type (task/snippet/etc.).
	var subType *string
	if req.IsTask {
		st := string(macrodb.DocumentSubTypeValueTask)
		subType = &st
		if _, err := tx.Exec(ctx,
			`INSERT INTO document_sub_type (document_id, sub_type) VALUES ($1, $2)`,
			docID, st); err != nil {
			return createdDocument{}, err
		}
		// NOTE: team task-number allocation (allocate_team_task_number) is
		// not ported — tasks created via POST /create_task need it. TODO.
	}

	// Version row.
	var versionID int64
	if fileType != nil && *fileType == "docx" {
		err = tx.QueryRow(ctx, `
			INSERT INTO "DocumentBom" ("documentId", "createdAt", "updatedAt")
			VALUES ($1, $2, $2) RETURNING id
		`, docID, pgtype.Timestamp{Time: createdAt, Valid: true}).Scan(&versionID)
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO "DocumentInstance" ("documentId", "sha", "createdAt", "updatedAt")
			VALUES ($1, $2, $3, $3) RETURNING id
		`, docID, req.Sha, pgtype.Timestamp{Time: createdAt, Valid: true}).Scan(&versionID)
	}
	if err != nil {
		return createdDocument{}, err
	}

	// Share permission: md files default to PUBLIC link + edit; others none.
	// TODO(port): team_default link-share resolution is not applied.
	var linkShare *string
	var linkShareLevel *string
	if fileType != nil && *fileType == "md" {
		ls := "PUBLIC"
		lv := "edit"
		linkShare, linkShareLevel = &ls, &lv
	}
	var sharePermID string
	err = tx.QueryRow(ctx, `
		INSERT INTO "SharePermission" ("linkShare", "linkShareAccessLevel", "createdAt", "updatedAt")
		VALUES ($1, $2, NOW(), NOW()) RETURNING id
	`, linkShare, linkShareLevel).Scan(&sharePermID)
	if err != nil {
		return createdDocument{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "DocumentPermission" ("documentId", "sharePermissionId") VALUES ($1, $2)
	`, docID, sharePermID); err != nil {
		return createdDocument{}, err
	}

	if !req.SkipHistory {
		if _, err := tx.Exec(ctx, `
			INSERT INTO "UserHistory" ("userId", "itemId", "itemType", "createdAt", "updatedAt")
			VALUES ($1, $2, 'document', $3, $3)
			ON CONFLICT ("userId", "itemId", "itemType") DO UPDATE SET "updatedAt" = $3
		`, userID, docID, pgtype.Timestamp{Time: createdAt, Valid: true}); err != nil {
			return createdDocument{}, err
		}
	}

	// Owner grant in entity_access + entity registry row.
	if _, err := tx.Exec(ctx, `
		INSERT INTO entity_access (entity_id, entity_type, source_id, source_type, access_level)
		VALUES ($1::uuid, 'document', $2, 'user', 'owner')
	`, docID, userID); err != nil {
		return createdDocument{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO entity (id, entity_type, owner_type, owner_id)
		VALUES ($1::uuid, 'document', 'user', $2)
		ON CONFLICT (id) DO NOTHING
	`, docID, userID); err != nil {
		return createdDocument{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return createdDocument{}, err
	}
	return createdDocument{documentID: docID, versionID: versionID, createdAt: &createdAt, subType: subType}, nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// editDocument mirrors PATCH /documents/{document_id} (documents hex
// edit_document) — rename, project move, file type change, and basic
// link-share updates.
func (s *Service) editDocument(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	var req struct {
		DocumentName    *string          `json:"documentName"`
		ProjectID       *string          `json:"projectId"`
		FileType        *string          `json:"fileType"`
		SharePermission *sharePermission `json:"sharePermission"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.DocumentName != nil && utf8.RuneCountInString(*req.DocumentName) > maxDocumentNameRunes {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"code":      "DOCUMENT_NAME_TOO_LONG",
			"maxLength": maxDocumentNameRunes,
			"message":   "name too long",
		})
		return
	}
	ctx := r.Context()
	level, err := s.accessLevelFor(ctx, caller, EntityTypeDocument, docID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	if level < AccessLevelEdit {
		httpx.ErrorJSON(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// Project moves and share-permission edits require owner.
	if (req.ProjectID != nil || req.SharePermission != nil) && level != AccessLevelOwner {
		httpx.ErrorJSON(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	basic, err := s.q.GetBasicDocument(ctx, docID)
	if isNoRows(err) {
		httpx.ErrorJSON(w, http.StatusNotFound, fmt.Sprintf("document with id %q was not found", docID))
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get document")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to edit document")
		return
	}
	defer tx.Rollback(ctx)

	sets := []string{}
	args := []any{docID}
	if req.DocumentName != nil {
		name := *req.DocumentName
		if cleaned, _ := cleanDocumentName(name); cleaned != name {
			name = cleaned
		}
		sets = append(sets, fmt.Sprintf(`"name" = $%d`, len(args)+1))
		args = append(args, name)
	}
	if req.ProjectID != nil {
		sets = append(sets, fmt.Sprintf(`"projectId" = $%d`, len(args)+1))
		if *req.ProjectID == "" {
			args = append(args, nil)
		} else {
			args = append(args, *req.ProjectID)
		}
	}
	if req.FileType != nil {
		sets = append(sets, fmt.Sprintf(`"fileType" = $%d`, len(args)+1))
		args = append(args, *req.FileType)
	}
	sets = append(sets, `"updatedAt" = NOW()`)
	if _, err := tx.Exec(ctx,
		`UPDATE "Document" SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to edit document")
		return
	}

	// Basic share-permission update (link share only; channel/team grants
	// are a deeper port — TODO).
	if req.SharePermission != nil {
		spID, err := s.q.GetSharePermissionId(ctx, docID)
		if err == nil && spID != "" {
			var ls, lsl *string
			if req.SharePermission.LinkShare != nil {
				v := strings.ToUpper(*req.SharePermission.LinkShare)
				ls = &v
			}
			if req.SharePermission.LinkShareAccessLevel != nil {
				v := strings.ToLower(*req.SharePermission.LinkShareAccessLevel)
				lsl = &v
			}
			if _, err := tx.Exec(ctx, `
				UPDATE "SharePermission" SET "linkShare" = $2, "linkShareAccessLevel" = $3, "updatedAt" = NOW()
				WHERE id = $1
			`, spID, ls, lsl); err != nil {
				httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to update share permission")
				return
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to edit document")
		return
	}

	actorID := caller.UserID
	prevProject := textPtr(basic.ProjectID)
	s.publishDocumentEvent(docID, DocEventUpdated, DocumentUpdatedMetadata{
		DocumentID:             docID,
		Owner:                  basic.Owner,
		ActorUserID:            &actorID,
		DocumentName:           req.DocumentName,
		PreviousProjectID:      prevProject,
		ProjectID:              req.ProjectID,
		SharePermissionUpdated: req.SharePermission != nil,
	})

	httpx.WriteJSON(w, http.StatusOK, okData(struct{}{}))
}

// sharePermission mirrors the update-share-permission request (subset).
type sharePermission struct {
	LinkShare            *string `json:"linkShare"`
	LinkShareAccessLevel *string `json:"linkShareAccessLevel"`
}

// deleteDocument mirrors DELETE /documents/{document_id} (soft delete).
func (s *Service) deleteDocument(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelOwner); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			httpx.ErrorJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	basic, err := s.q.GetBasicDocument(ctx, docID)
	if isNoRows(err) {
		httpx.ErrorJSON(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	if basic.SubType.Valid && basic.SubType.DocumentSubTypeValue == macrodb.DocumentSubTypeValueInitiativeDescription {
		httpx.ErrorJSON(w, http.StatusBadRequest, "initiative description documents cannot be deleted")
		return
	}
	tag, err := s.pool.Exec(ctx, `UPDATE "Document" SET "deletedAt" = NOW() WHERE id = $1`, docID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to delete document")
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.ErrorJSON(w, http.StatusNotFound, "document not found")
		return
	}
	actorID := caller.UserID
	s.publishDocumentEvent(docID, DocEventDeleted, DocumentDeletedMetadata{
		DocumentID:  docID,
		ActorUserID: &actorID,
		ProjectID:   textPtr(basic.ProjectID),
	})
	httpx.WriteJSON(w, http.StatusOK, okData(struct{}{}))
}

// revertDeleteDocument mirrors PUT /documents/{document_id}/revert_delete.
func (s *Service) revertDeleteDocument(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	info, err := s.q.GetDeletedDocumentInfo(ctx, docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	// Only the owner may revert a delete.
	if info.Owner != caller.UserID && !caller.Internal {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, err := s.q.RevertDeleteDocument(ctx, docID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to revert delete")
		return
	}
	if err := s.q.RevertDeleteDocument2(ctx, docID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to revert delete")
		return
	}
	actorID := caller.UserID
	s.publishDocumentEvent(docID, DocEventUpdated, DocumentUpdatedMetadata{
		DocumentID:  docID,
		Owner:       info.Owner,
		ActorUserID: &actorID,
	})
	writeOK(w, struct{}{})
}

// permanentlyDeleteDocument mirrors DELETE /documents/{document_id}/permanent.
func (s *Service) permanentlyDeleteDocument(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelOwner); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete document")
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	// share permission rows
	if spID, err := qtx.DeleteDocument(ctx, docID); err == nil && spID != "" {
		_, _ = tx.Exec(ctx, `DELETE FROM "DocumentPermission" WHERE "documentId" = $1`, docID)
		_ = qtx.DeleteSharePermission(ctx, spID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM "DocumentInstanceModificationData" WHERE "documentInstanceId" IN (SELECT id FROM "DocumentInstance" WHERE "documentId" = $1)`, docID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete document")
		return
	}
	for _, q := range []string{
		`DELETE FROM "DocumentInstance" WHERE "documentId" = $1`,
		`DELETE FROM "BomPart" WHERE "documentBomId" IN (SELECT id FROM "DocumentBom" WHERE "documentId" = $1)`,
		`DELETE FROM "DocumentBom" WHERE "documentId" = $1`,
		`DELETE FROM document_sub_type WHERE document_id = $1`,
		`DELETE FROM "UserHistory" WHERE "itemId" = $1 AND "itemType" = 'document'`,
		`DELETE FROM entity_access WHERE entity_id::text = $1 AND entity_type = 'document'`,
		`DELETE FROM entity WHERE id::text = $1 AND entity_type = 'document'`,
	} {
		if _, err := tx.Exec(ctx, q, docID); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to delete document")
			return
		}
	}
	if err := qtx.DeleteDocument2(ctx, docID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete document")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete document")
		return
	}

	// Remove stored objects (best-effort; Rust deletes S3 keys too).
	basic, err := s.q.GetBasicDocument(ctx, docID)
	_ = basic // document row is gone; prefix uses the caller owner when known
	_ = err
	owner := caller.UserID
	_ = s.store.DeleteByPrefix(ctx, s.bucket, owner+"/"+docID)

	s.publishDocumentEvent(docID, DocEventPurged, DocumentPurgedMetadata{DocumentID: docID})
	writeOK(w, struct{}{})
}

// saveDocument mirrors PUT /documents/{document_id} — creates a new version
// (instance sha or docx BOM) and returns metadata + presigned upload URL.
func (s *Service) saveDocument(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	var req struct {
		Sha              *string          `json:"sha"`
		NewBom           []saveBomPart    `json:"newBom"`
		ModificationData *json.RawMessage `json:"modificationData"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelEdit); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			httpx.ErrorJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	basic, err := s.q.GetBasicDocument(ctx, docID)
	if isNoRows(err) {
		httpx.ErrorJSON(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	fileType := textPtr(basic.FileType)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to save document")
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `UPDATE "Document" SET "updatedAt" = NOW() WHERE id = $1`, docID); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to save document")
		return
	}

	var versionID int64
	var bomJSON []byte
	if fileType != nil && *fileType == "docx" {
		if err := tx.QueryRow(ctx,
			`INSERT INTO "DocumentBom" ("documentId") VALUES ($1) RETURNING id`,
			docID).Scan(&versionID); err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to save document")
			return
		}
		for _, part := range req.NewBom {
			// The sha becomes the content-addressed object key that
			// location_v3 presigns — reject anything that is not a safe
			// single-segment key so a malicious BOM can never be turned
			// into a presigned URL for an arbitrary bucket object.
			if !validBomSHA(part.Sha) {
				httpx.ErrorJSON(w, http.StatusBadRequest, "invalid bom part sha")
				return
			}
			// The path is a caller-supplied zip-internal name: it is
			// persisted and echoed back in location responses, so it must
			// not be absolute or contain traversal segments.
			if !validBomPath(part.Path) {
				httpx.ErrorJSON(w, http.StatusBadRequest, "invalid bom part path")
				return
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO "BomPart" ("documentBomId", sha, path) VALUES ($1, $2, $3)`,
				versionID, part.Sha, part.Path); err != nil {
				httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to save document")
				return
			}
		}
		bomJSON, _ = json.Marshal(req.NewBom)
	} else {
		sha := ""
		if req.Sha != nil {
			sha = *req.Sha
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO "DocumentInstance" ("documentId", "sha") VALUES ($1, $2) RETURNING id`,
			docID, sha).Scan(&versionID); err != nil {
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to save document")
			return
		}
		if req.ModificationData != nil {
			if _, err := tx.Exec(ctx,
				`INSERT INTO "DocumentInstanceModificationData" ("documentInstanceId", "modificationData") VALUES ($1, $2)`,
				versionID, []byte(*req.ModificationData)); err != nil {
				httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to save document")
				return
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to save document")
		return
	}

	// Presigned PUT for the new version (non-docx uploads the whole file;
	// docx parts upload per-sha — handled by the upload pipeline).
	var presigned *string
	if fileType == nil || *fileType != "docx" {
		contentType := "application/octet-stream"
		if fileType != nil {
			contentType = mimeTypeFor(*fileType)
		}
		key := fmt.Sprintf("%s/%s/%d", basic.Owner, docID, versionID)
		if url, err := s.store.PresignPut(ctx, s.bucket, key, contentType, presignTTL); err == nil {
			presigned = &url
		}
	}

	meta := DocumentResponseMetadata{
		DocumentID:            docID,
		DocumentVersionID:     versionID,
		Owner:                 Owner(basic.Owner),
		DocumentName:          basic.DocumentName,
		FileType:              fileType,
		Sha:                   req.Sha,
		BranchedFromID:        textPtr(basic.BranchedFromID),
		BranchedFromVersionID: int8Ptr(basic.BranchedFromVersionID),
		DocumentFamilyID:      int8Ptr(basic.DocumentFamilyID),
	}
	if len(bomJSON) > 0 {
		meta.DocumentBom = bomJSON
	}
	now := time.Now().UTC()
	meta.UpdatedAt = &now

	resp := struct {
		DocumentMetadata DocumentResponseMetadata `json:"documentMetadata"`
		PresignedURL     *string                  `json:"presignedUrl,omitempty"`
	}{DocumentMetadata: meta, PresignedURL: presigned}

	s.publishDocumentEvent(docID, DocEventUpdated, DocumentUpdatedMetadata{
		DocumentID:  docID,
		Owner:       basic.Owner,
		ActorUserID: &caller.UserID,
	})
	httpx.WriteJSON(w, http.StatusOK, okData(resp))
}

type saveBomPart struct {
	Sha  string `json:"sha"`
	Path string `json:"path"`
}

// getDocumentPermissions mirrors GET /documents/{document_id}/permissions.
func (s *Service) getDocumentPermissions(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelOwner); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	row, err := s.q.GetDocumentSharePermission(ctx, docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "share permission not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get permissions")
		return
	}
	resp := map[string]any{
		"id":    row.ID,
		"owner": row.Owner,
	}
	if row.LinkShare.Valid {
		resp["linkShare"] = row.LinkShare.String
	}
	if row.LinkShareAccessLevel.Valid {
		resp["linkShareAccessLevel"] = string(row.LinkShareAccessLevel.AccessLevel)
	}
	if row.TeamShareAccessLevel.Valid {
		resp["teamShareAccessLevel"] = string(row.TeamShareAccessLevel.AccessLevel)
	}
	if len(row.ChannelSharePermissions) > 0 {
		resp["channelSharePermissions"] = json.RawMessage(row.ChannelSharePermissions)
	}
	writeOK(w, map[string]any{"documentPermissions": resp})
}

// getDocumentAccessLevel mirrors GET /documents/{document_id}/access_level.
func (s *Service) getDocumentAccessLevel(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	level, err := s.accessLevelFor(r.Context(), caller, EntityTypeDocument, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to get user access level")
		return
	}
	if level == AccessLevelNone {
		writeErr(w, http.StatusUnauthorized, "user does not have access to document")
		return
	}
	writeOK(w, map[string]any{"userAccessLevel": level.String()})
}

// listDocumentsWithAccess mirrors GET /internal/documents/list_with_access.
func (s *Service) listDocumentsWithAccess(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	page := queryInt(r, "page", 0)
	pageSize := queryInt(r, "pageSize", 50)
	if pageSize <= 0 || pageSize > 1000 {
		writeErr(w, http.StatusBadRequest, "page_size must be between 1 and 1000")
		return
	}
	if page < 0 {
		writeErr(w, http.StatusBadRequest, "page must be non-negative")
		return
	}
	var fileTypes []string
	if ft := r.URL.Query().Get("fileTypes"); ft != "" {
		fileTypes = strings.Split(ft, ",")
	}
	minLevel := r.URL.Query().Get("minAccessLevel")
	if minLevel == "" {
		minLevel = "view"
	}
	rows, err := s.q.ListDocumentsWithAccess(r.Context(), macrodb.ListDocumentsWithAccessParams{
		UserID:  caller.UserID,
		Column2: fileTypes,
		Column3: minLevel,
		Limit:   int32(pageSize),
		Offset:  int32(page * pageSize),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to list documents")
		return
	}
	out := make([]ListDocumentsWithAccessRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, ListDocumentsWithAccessRow{
			DocumentID:   row.DocumentID,
			DocumentName: row.DocumentName,
			Owner:        Owner(row.Owner),
			FileType:     textPtr(row.FileType),
			ProjectID:    textPtr(row.ProjectID),
			CreatedAt:    timePtr(row.CreatedAt),
			UpdatedAt:    timePtr(row.UpdatedAt),
			DeletedAt:    timePtr(row.DeletedAt),
			AccessLevel:  row.AccessLevel,
		})
	}
	// Rust returns the bare ListDocumentsWithAccessResponse (no envelope):
	// {"documents": [...], "resultsReturned": n} — items are snake_case.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"documents":       out,
		"resultsReturned": len(out),
	})
}
