package dss

// documents2.go holds the remaining document handlers: list, preview, text,
// location (presigned urls), export, version lookups, processing results,
// simple_save / presave, starter docs, and permission tokens.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// ---------- misc document lookups ----------

// getDocumentList mirrors GET /documents/list — the caller's own documents
// in the compact GetDocumentListResult shape.
func (s *Service) getDocumentList(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	rows, err := s.q.GetDocumentList(r.Context(), caller.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document list")
		return
	}
	out := make([]GetDocumentListResult, 0, len(rows))
	for _, row := range rows {
		out = append(out, GetDocumentListResult{
			DocumentID:            row.DocumentID,
			DocumentVersionID:     row.DocumentVersionID,
			DocumentName:          row.DocumentName,
			FileType:              textPtr(row.FileType),
			BranchedFromID:        textPtr(row.BranchedFromID),
			BranchedFromVersionID: int8Ptr(row.BranchedFromVersionID),
			DocumentFamilyID:      int8Ptr(row.DocumentFamilyID),
			CreatedAt:             timePtr(row.CreatedAt),
			UpdatedAt:             timePtr(row.UpdatedAt),
		})
	}
	writeOK(w, out)
}

// getDocumentBasic mirrors GET /internal/documents/{document_id}/basic —
// DocumentBasic (snake_case) plus project name.
func (s *Service) getDocumentBasic(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	// Per-document authorization: this handler also serves the /internal
	// surface, where an internal key alone (or any user JWT) must not
	// expose metadata for documents the acting identity cannot view.
	if _, err := s.requireAccess(r.Context(), caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	row, err := s.q.GetBasicDocument(r.Context(), docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("document with id %q was not found", docID))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	var subType *string
	if row.SubType.Valid {
		st := string(row.SubType.DocumentSubTypeValue)
		subType = &st
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"document_id":              docID,
		"document_name":            row.DocumentName,
		"owner":                    row.Owner,
		"file_type":                textPtr(row.FileType),
		"sub_type":                 subType,
		"branched_from_id":         textPtr(row.BranchedFromID),
		"branched_from_version_id": int8Ptr(row.BranchedFromVersionID),
		"document_family_id":       int8Ptr(row.DocumentFamilyID),
		"project_id":               textPtr(row.ProjectID),
		"deleted_at":               timePtr(row.DeletedAt),
	})
}

// getDocumentVersion mirrors GET /documents/{document_id}/{document_version_id}.
func (s *Service) getDocumentVersion(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	versionID, err := strconv.ParseInt(chiParam(r, "document_version_id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid document version id")
		return
	}
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
	row, err := s.q.GetDocumentVersion(ctx, macrodb.GetDocumentVersionParams{ID: docID, ID_2: versionID})
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "unable to get document")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	var subType *string
	if row.SubType.Valid {
		st := string(row.SubType.DocumentSubTypeValue)
		subType = &st
	}
	var projectName *string
	if row.ProjectName != "" {
		projectName = &row.ProjectName
	}
	var sha *string
	if row.Sha != "" {
		sha = &row.Sha
	}
	httpx.WriteJSON(w, http.StatusOK, okData(GetDocumentResponseData{
		DocumentMetadata: DocumentMetadata{
			DocumentID:            row.DocumentID,
			DocumentVersionID:     row.DocumentVersionID,
			Owner:                 Owner(row.Owner),
			DocumentName:          row.DocumentName,
			FileType:              textPtr(row.FileType),
			Sha:                   sha,
			ProjectID:             textPtr(row.ProjectID),
			ProjectName:           projectName,
			BranchedFromID:        textPtr(row.BranchedFromID),
			BranchedFromVersionID: int8Ptr(row.BranchedFromVersionID),
			DocumentFamilyID:      int8Ptr(row.DocumentFamilyID),
			DocumentBom:           bytesToRaw(row.DocumentBom),
			ModificationData:      bytesToRaw(row.ModificationData),
			CreatedAt:             timePtr(row.CreatedAt),
			UpdatedAt:             timePtr(row.UpdatedAt),
			DeletedAt:             timePtr(row.DeletedAt),
			SubType:               subType,
		},
		UserAccessLevel: level.String(),
	}))
}

// ---------- object keys / locations ----------

// documentKey builds {owner}/{document_id}/{document_version_id}.
func documentKey(owner, docID string, versionID int64) string {
	return fmt.Sprintf("%s/%s/%d", owner, docID, versionID)
}

// convertedPDFKey mirrors build_docx_to_pdf_converted_document_key:
// {owner}/{document_id}/converted.pdf
func convertedPDFKey(owner, docID string) string {
	return fmt.Sprintf("%s/%s/converted.pdf", owner, docID)
}

// latestVersionID resolves the newest DocumentInstance/DocumentBom id.
func (s *Service) latestVersionID(r *http.Request, docID string, fileType *string) (int64, error) {
	ctx := r.Context()
	if fileType != nil && *fileType == "docx" {
		return s.q.GetLatestDocumentBomVersionId(ctx, docID)
	}
	row, err := s.q.GetLatestDocumentVersionId(ctx, docID)
	return row.ID, err
}

// getDocumentKey mirrors GET /documents/{document_id}/{document_version_id}/key
// (get_document_key.rs) — returns the storage key, not a URL.
func (s *Service) getDocumentVersionKey(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	versionID, err := strconv.ParseInt(chiParam(r, "document_version_id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid document version id")
		return
	}
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	basic, err := s.q.GetBasicDocument(ctx, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	key := documentKey(basic.Owner, docID, versionID)
	if ft := textPtr(basic.FileType); ft != nil && *ft == "docx" {
		key = convertedPDFKey(basic.Owner, docID)
	}
	writeOK(w, map[string]any{"key": key})
}

// getDocumentKey mirrors GET /documents/{document_id}/key — the latest
// version's storage key.
func (s *Service) getDocumentKey(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	basic, err := s.q.GetBasicDocument(ctx, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	fileType := textPtr(basic.FileType)
	key := ""
	if fileType != nil && *fileType == "docx" {
		key = convertedPDFKey(basic.Owner, docID)
	} else {
		vid, err := s.latestVersionID(r, docID, fileType)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get document version")
			return
		}
		key = documentKey(basic.Owner, docID, vid)
	}
	writeOK(w, map[string]any{"key": key})
}

// getDocumentLocation mirrors GET /documents/{document_id}/location — for
// the self-hosted port it behaves like location_v3's single-url variant.
func (s *Service) getDocumentLocation(w http.ResponseWriter, r *http.Request) {
	s.getDocumentLocationV3(w, r)
}

// getDocumentLocationV3 mirrors GET /documents/{document_id}/location_v3.
// LocationResponseV3 is an untagged enum (camelCase):
//
//	{"presignedUrl"|"presigned_url": ..., "metadata": {DocumentBasic}}
//
// Rust emits snake_case inside the enum variants; we keep that.
func (s *Service) getDocumentLocationV3(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	basic, err := s.q.GetBasicDocument(ctx, docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	fileType := textPtr(basic.FileType)
	basicJSON := map[string]any{
		"document_id":              docID,
		"document_name":            basic.DocumentName,
		"owner":                    basic.Owner,
		"file_type":                fileType,
		"branched_from_id":         textPtr(basic.BranchedFromID),
		"branched_from_version_id": int8Ptr(basic.BranchedFromVersionID),
		"document_family_id":       int8Ptr(basic.DocumentFamilyID),
		"project_id":               textPtr(basic.ProjectID),
		"deleted_at":               timePtr(basic.DeletedAt),
	}
	if basic.SubType.Valid {
		basicJSON["sub_type"] = string(basic.SubType.DocumentSubTypeValue)
	}

	// docx documents expose every BOM part plus the converted pdf.
	if fileType != nil && *fileType == "docx" {
		parts, err := s.q.GetBomParts(ctx, docID)
		if err != nil && !isNoRows(err) {
			writeErr(w, http.StatusInternalServerError, "unable to get document bom")
			return
		}
		urls := make([]map[string]any, 0, len(parts)+1)
		for _, p := range parts {
			// Rust stores docx parts content-addressed by sha and presigns
			// the sha — never the caller-supplied path, which is only a
			// zip-internal name (see document_shas / location.rs). The sha
			// is validated as a safe single-segment object key so a stored
			// part can never be turned into a presigned URL for an
			// arbitrary bucket object.
			if !validBomSHA(p.Sha) {
				writeErr(w, http.StatusInternalServerError, "invalid bom part")
				return
			}
			url, err := s.store.PresignGet(ctx, s.bucket, p.Sha, presignTTL)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "unable to generate presigned url")
				return
			}
			urls = append(urls, map[string]any{"sha": p.Sha, "key": p.Path, "url": url})
		}
		// The converted pdf of the whole docx (if rendered).
		if pdfURL, err := s.store.PresignGet(ctx, s.bucket, convertedPDFKey(basic.Owner, docID), presignTTL); err == nil {
			urls = append(urls, map[string]any{"key": convertedPDFKey(basic.Owner, docID), "url": pdfURL})
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"presigned_urls": urls,
			"metadata":       basicJSON,
		})
		return
	}

	vid, err := s.latestVersionID(r, docID, fileType)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document version")
		return
	}
	key := documentKey(basic.Owner, docID, vid)
	url, err := s.store.PresignGet(ctx, s.bucket, key, presignTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to generate presigned url")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"presigned_url": url,
		"metadata":      basicJSON,
	})
}

// ---------- text / views / preview ----------

// getDocumentText mirrors GET /documents/{document_id}/text — extracted text
// lives in the "DocumentText" table (populated by the worker pipeline).
func (s *Service) getDocumentText(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	row, err := s.q.GetPdfDocxDocumentText(ctx, docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "Document text not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to retrieve document text")
		return
	}
	// Rust returns {"text": "..."} as a bare Json body.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"text": row.Content})
}

// getDocumentViews mirrors GET /documents/{document_id}/views — bare
// UserViewsResponse {users: [emails], count}.
func (s *Service) getDocumentViews(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	users, err := s.q.GetDocumentViews(ctx, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document views")
		return
	}
	count, err := s.q.GetDocumentViewCount(ctx, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document view count")
		return
	}
	if users == nil {
		users = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": users, "count": count})
}

// GetBatchPreviewRequest mirrors model::document::preview::GetBatchPreviewRequest.
type GetBatchPreviewRequest struct {
	DocumentIDs []string `json:"document_ids"`
}

// getBatchPreview mirrors POST /documents/preview — returns a bare
// {"previews": [...]} where each entry is the internally-tagged
// DocumentPreview enum: {"type":"access",...} | {"type":"no_access",...} |
// {"type":"does_not_exist",...}.
func (s *Service) getBatchPreview(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req GetBatchPreviewRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	previews := make([]any, 0, len(req.DocumentIDs))
	for _, docID := range req.DocumentIDs {
		basic, err := s.q.GetBasicDocument(ctx, docID)
		if err != nil {
			previews = append(previews, map[string]any{"type": "does_not_exist", "document_id": docID})
			continue
		}
		level, err := s.accessLevelFor(ctx, caller, EntityTypeDocument, docID)
		if err != nil || level < AccessLevelView {
			previews = append(previews, map[string]any{"type": "no_access", "document_id": docID})
			continue
		}
		p := map[string]any{
			"type":          "access",
			"document_id":   docID,
			"document_name": basic.DocumentName,
			"owner":         basic.Owner,
			"file_type":     textPtr(basic.FileType),
		}
		if basic.SubType.Valid {
			st := string(basic.SubType.DocumentSubTypeValue)
			// DocumentPreviewDataSubType is internally tagged too:
			// {"type":"task","is_completed":bool} etc. is_completed needs the
			// entity_properties join (BatchGetDocumentPreviewV22) — TODO; the
			// property-definition id is per-team and not yet ported.
			p["sub_type"] = map[string]any{"type": st}
		}
		previews = append(previews, p)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"previews": previews})
}

// ---------- export / processing ----------

// exportDocument mirrors GET /documents/{document_id}/export — presigned GET
// for the document's exportable blob (converted pdf for docx, raw blob for
// pdf/static files).
func (s *Service) exportDocument(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	basic, err := s.q.GetBasicDocument(ctx, docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	fileType := textPtr(basic.FileType)
	var key string
	switch {
	case fileType != nil && *fileType == "docx":
		key = convertedPDFKey(basic.Owner, docID)
	default:
		vid, err := s.latestVersionID(r, docID, fileType)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to get document version")
			return
		}
		key = documentKey(basic.Owner, docID, vid)
	}
	url, err := s.store.PresignGet(ctx, s.bucket, key, presignTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to generate presigned url")
		return
	}
	writeOK(w, map[string]any{"presigned_url": url})
}

// getDocumentProcessingResult mirrors GET /documents/{document_id}/processing
// — reads the Postgres processing-results table (job type "pdf_preprocess").
func (s *Service) getDocumentProcessingResult(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	content, err := s.q.GetDocumentProcessContent(ctx, macrodb.GetDocumentProcessContentParams{
		DocumentId: docID,
		JobType:    "pdf_preprocess",
	})
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "processing result not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get processing result")
		return
	}
	writeOK(w, map[string]any{"result": content})
}

// getJobProcessingResult mirrors GET /documents/{document_id}/processing/{job_id}.
func (s *Service) getJobProcessingResult(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	jobID := chiParam(r, "job_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	content, err := s.q.GetDocumentProcessContentFromJobId(ctx, macrodb.GetDocumentProcessContentFromJobIdParams{
		JobId:      jobID,
		DocumentId: docID,
	})
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "processing result not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get processing result")
		return
	}
	writeOK(w, map[string]any{"result": content})
}

// getFullPDFModificationData mirrors GET
// /internal/documents/{document_id}/full_pdf_modification_data — pdf anchors
// and allowable edits for the document's annotation threads.
func (s *Service) getFullPDFModificationData(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	rows, err := s.q.GetCompletePdfModificationData(ctx, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get modification data")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"thread_id":           row.ThreadID,
			"is_resolved":         row.IsResolved,
			"anchor_uuid":         row.AnchorUuid,
			"page":                row.Page,
			"original_page":       row.OriginalPage,
			"original_index":      row.OriginalIndex,
			"should_lock_on_save": row.ShouldLockOnSave,
			"was_edited":          row.WasEdited,
			"was_deleted":         row.WasDeleted,
			"allowable_edits":     json.RawMessage(row.AllowableEdits),
			"x_pct":               row.XPct,
			"y_pct":               row.YPct,
			"width_pct":           row.WidthPct,
			"height_pct":          row.HeightPct,
			"rotation":            row.Rotation,
		})
	}
	writeOK(w, map[string]any{"modification_data": out})
}

// ---------- simple save / presave ----------

// simpleSave mirrors PUT /documents/{document_id}/simple_save — multipart
// upload stored directly at the new version key; returns updated metadata.
func (s *Service) simpleSave(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelEdit); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	basic, err := s.q.GetBasicDocument(ctx, docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	if basic.DeletedAt.Valid {
		writeErr(w, http.StatusBadRequest, "cannot modify a deleted document")
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "expected multipart form")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing 'file' form part")
		return
	}
	defer file.Close()

	// New DocumentInstance row, then store the blob at {owner}/{doc}/{ver}.
	var versionID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO "DocumentInstance" ("documentId", "createdAt", "updatedAt")
		 VALUES ($1, NOW(), NOW()) RETURNING id`, docID).Scan(&versionID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to save document")
		return
	}
	key := documentKey(basic.Owner, docID, versionID)
	contentType := "application/octet-stream"
	if ft := textPtr(basic.FileType); ft != nil {
		contentType = mimeTypeFor(*ft)
	}
	if err := s.store.PutObject(ctx, s.bucket, key, file, contentType); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to store document")
		return
	}
	_, _ = s.pool.Exec(ctx, `UPDATE "Document" SET "updatedAt" = NOW(), uploaded = true WHERE id = $1`, docID)

	s.publishDocumentEvent(docID, DocEventUpdated, DocumentUpdatedMetadata{
		DocumentID:  docID,
		Owner:       basic.Owner,
		ActorUserID: &caller.UserID,
	})

	row, err := s.q.GetDocument2(ctx, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	meta := mapDocumentMetadata(row)
	httpx.WriteJSON(w, http.StatusOK, okData(DocumentResponse{
		DocumentMetadata: docResponseMetadata(meta),
	}))
}

// PreSaveDocumentRequest mirrors model::document::PreSaveDocumentRequest.
type PreSaveDocumentRequest struct {
	Sha              *string          `json:"sha"`
	FileType         *string          `json:"fileType"`
	NewBom           []saveBomPart    `json:"newBom"`
	ModificationData *json.RawMessage `json:"modificationData"`
}

// preSave mirrors PUT /documents/presave/{document_id} (and the
// /{document_id}/presave alias) — creates the pending version row and
// returns a presigned PUT url for the blob.
func (s *Service) preSave(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	var req PreSaveDocumentRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelEdit); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	basic, err := s.q.GetBasicDocument(ctx, docID)
	if isNoRows(err) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get document")
		return
	}
	if basic.DeletedAt.Valid {
		writeErr(w, http.StatusBadRequest, "cannot modify a deleted document")
		return
	}
	var versionID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO "DocumentInstance" ("documentId", sha, "createdAt", "updatedAt")
		 VALUES ($1, $2, NOW(), NOW()) RETURNING id`, docID, req.Sha).Scan(&versionID); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to presave document")
		return
	}
	key := documentKey(basic.Owner, docID, versionID)
	contentType := "application/octet-stream"
	if req.FileType != nil {
		contentType = mimeTypeFor(*req.FileType)
	}
	url, err := s.store.PresignPut(ctx, s.bucket, key, contentType, presignTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to generate presigned url")
		return
	}
	// Rust PreSaveDocumentResponse: document_id + presigned urls per part.
	// For editable files there is a single file url.
	writeOK(w, map[string]any{
		"document_id":    docID,
		"presigned_urls": []map[string]any{{"key": key, "url": url}},
		"presigned_url":  url,
	})
}

// ---------- starter docs ----------

// starterDocNamespace mirrors STARTER_DOC_ID_NAMESPACE.
var starterDocNamespace = uuid.MustParse("3d1c5f86-9e4b-45d2-a7c8-6b0e2f9a1d47")

// starterDocID mirrors starter_doc_id: UUIDv5(namespace, "{user}:{name}").
func starterDocID(userID, documentName string) string {
	return uuid.NewSHA1(starterDocNamespace, []byte(userID+":"+documentName)).String()
}

const howToGuideName = "Macro how to guide"

// getStarterDocs mirrors GET /documents/starter_docs.
func (s *Service) getStarterDocs(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	// Bare Json body in Rust (no envelope).
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"how_to_guide_id": starterDocID(caller.UserID, howToGuideName),
	})
}

// initializeUserDocuments mirrors POST /documents/initialize_user_documents:
// find-or-create the caller's "Macro how to guide" markdown document with the
// deterministic starter id. The Rust version seeds real guide content; the
// self-hosted port creates an empty markdown document (content seeding TODO).
func (s *Service) initializeUserDocuments(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	docID := starterDocID(caller.UserID, howToGuideName)
	if _, err := s.q.GetBasicDocument(ctx, docID); err == nil {
		writeOK(w, map[string]any{"success": true})
		return
	} else if !isNoRows(err) {
		writeErr(w, http.StatusInternalServerError, "unable to initialize documents")
		return
	}
	md := "md"
	_, err := s.createDocumentTx(ctx, caller.UserID, CreateDocumentRequest{
		ID:           &docID,
		DocumentName: howToGuideName,
		FileType:     &md,
	}, howToGuideName, &md)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to initialize documents")
		return
	}
	writeOK(w, map[string]any{"success": true})
}

// ---------- permission tokens ----------

// permissionTokenClaims mirrors PermissionTokenClaims in
// crates/documents/src/domain/permission_token.rs.
type permissionTokenClaims struct {
	UserID      *string `json:"user_id,omitempty"`
	DocumentID  string  `json:"document_id"`
	AccessLevel string  `json:"access_level"`
	Actor       *string `json:"actor,omitempty"`
	jwt.RegisteredClaims
}

const permissionTokenIssuer = "document_storage_service" // ISSUER
const permissionTokenTTL = time.Hour                     // TOKEN_TTL_SECS

func (s *Service) encodePermissionToken(userID *string, docID string, level AccessLevel, actor *string) (string, error) {
	if s.jwtSecret == "" {
		return "", errors.New("document permission jwt not configured")
	}
	claims := permissionTokenClaims{
		UserID:      userID,
		DocumentID:  docID,
		AccessLevel: level.String(),
		Actor:       actor,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    permissionTokenIssuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(permissionTokenTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.jwtSecret))
}

// createPermissionToken mirrors POST /documents/permissions_token/{document_id}
// — mints an HS256 token carrying the caller's effective access level.
func (s *Service) createPermissionToken(w http.ResponseWriter, r *http.Request) {
	docID := chiParam(r, "document_id")
	ctx := r.Context()
	caller, _ := callerFrom(r)
	var userID *string
	if caller.UserID != "" && !caller.Internal {
		u := caller.UserID
		userID = &u
	}
	level, err := s.accessLevelFor(ctx, caller, EntityTypeDocument, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	if level < AccessLevelView {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	token, err := s.encodePermissionToken(userID, docID, level, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to encode jwt")
		return
	}
	// Bare Json response in Rust: {"token": "..."}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"token": token})
}

// validatePermissionsToken mirrors POST /documents/permissions_token/validate
// — decodes the token and returns its claims.
func (s *Service) validatePermissionsToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if s.jwtSecret == "" {
		writeErr(w, http.StatusInternalServerError, "unable to decode jwt")
		return
	}
	var claims permissionTokenClaims
	_, err := jwt.ParseWithClaims(req.Token, &claims,
		func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method")
			}
			return []byte(s.jwtSecret), nil
		},
		jwt.WithIssuer(permissionTokenIssuer),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			writeErr(w, http.StatusUnauthorized, "jwt is expired")
			return
		}
		writeErr(w, http.StatusUnauthorized, "unable to decode jwt")
		return
	}
	// Rust returns the decoded DocumentPermissionsToken claims directly.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user_id":      claims.UserID,
		"document_id":  claims.DocumentID,
		"access_level": claims.AccessLevel,
		"exp":          claims.ExpiresAt.Unix(),
		"iss":          claims.Issuer,
	})
}

// ---------- create_* convenience endpoints ----------

// createMarkdown mirrors POST /documents/create_markdown — creates a markdown
// document and immediately stores the provided markdown blob as its first
// version.
func (s *Service) createMarkdown(w http.ResponseWriter, r *http.Request) {
	s.createInitializedDocument(w, r, "md", "")
}

// createSnippet mirrors POST /documents/create_snippet.
func (s *Service) createSnippet(w http.ResponseWriter, r *http.Request) {
	s.createInitializedDocument(w, r, "md", "snippet")
}

// createSkill mirrors POST /documents/create_skill.
func (s *Service) createSkill(w http.ResponseWriter, r *http.Request) {
	s.createInitializedDocument(w, r, "md", "skill")
}

// createTask mirrors POST /documents/create_task — a markdown document with
// the task sub_type.
func (s *Service) createTask(w http.ResponseWriter, r *http.Request) {
	s.createInitializedDocument(w, r, "md", "task")
}

// createInitializedDocument is the shared implementation of the
// create_markdown/snippet/skill/task endpoints: insert document + first
// version, store the content blob, mint a permission token.
func (s *Service) createInitializedDocument(w http.ResponseWriter, r *http.Request, fileType, subType string) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	var req struct {
		DocumentName string  `json:"documentName"`
		ProjectID    *string `json:"projectId"`
		Markdown     *string `json:"markdown"`
		Content      *string `json:"content"`
		SkipHistory  bool    `json:"skipHistory"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	if req.ProjectID != nil && *req.ProjectID != "" {
		if _, err := s.requireAccess(ctx, caller, EntityTypeProject, *req.ProjectID, AccessLevelEdit); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "unable to check project access")
			return
		}
	}
	ft := fileType
	created, err := s.createDocumentTx(ctx, caller.UserID, CreateDocumentRequest{
		DocumentName: req.DocumentName,
		ProjectID:    req.ProjectID,
		SkipHistory:  req.SkipHistory,
		IsTask:       subType == "task",
	}, req.DocumentName, &ft)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create document")
		return
	}
	if subType != "" && subType != "task" {
		_, _ = s.pool.Exec(ctx,
			`INSERT INTO document_sub_type (document_id, sub_type) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			created.documentID, subType)
	}

	// Store the markdown content as the first version blob.
	content := ""
	if req.Markdown != nil {
		content = *req.Markdown
	}
	if req.Content != nil {
		content = *req.Content
	}
	key := documentKey(caller.UserID, created.documentID, created.versionID)
	if err := s.store.PutObject(ctx, s.bucket, key, strings.NewReader(content), mimeTypeFor(ft)); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to store document content")
		return
	}
	_, _ = s.pool.Exec(ctx, `UPDATE "Document" SET uploaded = true WHERE id = $1`, created.documentID)

	s.publishDocumentEvent(created.documentID, DocEventCreated, DocumentCreatedMetadata{
		DocumentID:   created.documentID,
		Owner:        caller.UserID,
		DocumentName: req.DocumentName,
		FileType:     &ft,
		ProjectID:    req.ProjectID,
		SubType:      created.subType,
	})

	var token *string
	if t, err := s.encodePermissionToken(&caller.UserID, created.documentID, AccessLevelEdit, nil); err == nil {
		token = &t
	}
	// Bare Json response in Rust: CreateMarkdownDocumentResponse.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"document_id": created.documentID,
		"document_metadata": map[string]any{
			"documentId":        created.documentID,
			"documentVersionId": created.versionID,
			"owner":             caller.UserID,
			"documentName":      req.DocumentName,
			"fileType":          ft,
			"createdAt":         created.createdAt,
			"updatedAt":         created.createdAt,
		},
		"token": token,
	})
}

// getSystemSkills mirrors GET /documents/system_skills — built-in skills are
// seeded by an external pipeline in Rust; self-hosted returns an empty list.
func (s *Service) getSystemSkills(w http.ResponseWriter, r *http.Request) {
	if _, ok := callerOr401(w, r); !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"skills": []any{}})
}

// ---------- small lookups ----------

// getShortID mirrors GET /documents/{document_id}/short_id — Rust issues a
// separate nanoid; the self-hosted port returns the canonical id.
func (s *Service) getShortID(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"short_id": chiParam(r, "document_id")})
}

// getBranchName mirrors GET /documents/{document_id}/branch_name — git-backed
// documents are not part of the self-hosted port.
func (s *Service) getBranchName(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"branch_name": nil})
}

// getGithubPRs mirrors GET /documents/{document_id}/github_prs — github sync
// is not part of the self-hosted port.
func (s *Service) getGithubPRs(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"pull_requests": []any{}})
}

// getTeamShare / putTeamShare mirror the team_share endpoints: they resolve
// the document's link share for team members. Basic port: read/update the
// SharePermission linkShare level scoped to TEAM.
func (s *Service) getTeamShare(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	if _, err := s.requireAccess(r.Context(), caller, EntityTypeDocument, docID, AccessLevelView); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	row, err := s.q.GetDocumentSharePermission(r.Context(), docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get share permission")
		return
	}
	shared := row.LinkShare.Valid && row.LinkShare.String == "TEAM" && row.LinkShareAccessLevel.Valid
	writeOK(w, map[string]any{"shared_with_team": shared})
}

func (s *Service) putTeamShare(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOr401(w, r)
	if !ok {
		return
	}
	docID := chiParam(r, "document_id")
	var req struct {
		SharedWithTeam   bool    `json:"shared_with_team"`
		SharedWithTeamV2 *bool   `json:"sharedWithTeam"`
		AccessLevel      *string `json:"access_level"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	if _, err := s.requireAccess(ctx, caller, EntityTypeDocument, docID, AccessLevelOwner); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to check access")
		return
	}
	shared := req.SharedWithTeam
	if req.SharedWithTeamV2 != nil {
		shared = *req.SharedWithTeamV2
	}
	spID, err := s.q.GetSharePermissionId(ctx, docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get share permission")
		return
	}
	var linkShare, level *string
	if shared {
		ls := "TEAM"
		lv := "edit"
		if req.AccessLevel != nil {
			lv = *req.AccessLevel
		}
		linkShare, level = &ls, &lv
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE "SharePermission" SET "linkShare" = $2, "linkShareAccessLevel" = $3, "updatedAt" = NOW() WHERE id = $1`,
		spID, linkShare, level); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to update share permission")
		return
	}
	writeOK(w, map[string]any{"success": true})
}
