// Package staticfile ports services/static_file_service: file metadata plus
// presigned upload/download URLs backed by MinIO (pkg/objectstore) and a
// Postgres metadata table (replaces DynamoDB).
package staticfile

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/objectstore"
)

const (
	// getPresignTTL mirrors the 1-hour GET presign.
	getPresignTTL = time.Hour
	// putPresignTTL mirrors the 2-minute PUT presign.
	putPresignTTL = 2 * time.Minute
	// maxRequestSize mirrors MAX_REQUEST_SIZE in the Rust api.rs.
	maxRequestSize = 4096
	// maxBulkDelete mirrors BulkDeleteRequest::max_file_ids (DynamoDB batch
	// limit, kept for parity).
	maxBulkDelete = 100
)

// Deps are the static file service dependencies.
type Deps struct {
	Metadata MetadataStore
	Store    objectstore.Store
	// Bucket is STATIC_STORAGE_BUCKET.
	Bucket string
	// ServiceURL is STATIC_FILE_SERVICE_URL; used to build permalinks.
	ServiceURL string
}

// FileMetadata is the API response model (models_sfs::FileMetadata).
type FileMetadata struct {
	FileID        string `json:"file_id"`
	ContentType   string `json:"content_type"`
	IsUploaded    bool   `json:"is_uploaded"`
	ExtensionData any    `json:"extension_data,omitempty"`
	FileName      string `json:"file_name"`
	OwnerID       string `json:"owner_id"`
	S3Key         string `json:"s3_key"`
}

type putFileRequest struct {
	FileName      string `json:"file_name"`
	ContentType   string `json:"content_type,omitempty"`
	ExtensionData any    `json:"extension_data,omitempty"`
}

type putFileResponse struct {
	UploadURL    string `json:"upload_url"`
	FileLocation string `json:"file_location"`
	ID           string `json:"id"`
}

type bulkDeleteRequest struct {
	FileIDs []string `json:"file_ids"`
}

type deleteResult struct {
	FileID  string  `json:"file_id"`
	Success bool    `json:"success"`
	Error   *string `json:"error,omitempty"`
}

type bulkDeleteResponse struct {
	Total     int            `json:"total"`
	Succeeded int            `json:"succeeded"`
	Failed    int            `json:"failed"`
	Results   []deleteResult `json:"results"`
}

// Register installs the /file routes on r (mounted under /api and /internal
// by the caller). Mirrors file::router() in the Rust service.
func (d Deps) Register(r chi.Router) {
	// RequestBodyLimitLayer(MAX_REQUEST_SIZE)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Body = http.MaxBytesReader(w, req.Body, maxRequestSize)
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/file/metadata/{file_id}", d.getMetadata)
	r.Get("/file/{file_id}/presigned-url", d.getPresignedURL)
	r.Put("/file", d.putPresignedURL)
	r.Delete("/file/{file_id}", d.deleteFile)
	r.Post("/file/bulk-delete", d.bulkDeleteFile)
}

func callerOf(w http.ResponseWriter, r *http.Request) (auth.Caller, bool) {
	c, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return auth.Caller{}, false
	}
	return c, true
}

func (d Deps) getMetadata(w http.ResponseWriter, r *http.Request) {
	if _, ok := callerOf(w, r); !ok {
		return
	}
	m, err := d.Metadata.GetMetadata(r.Context(), chi.URLParam(r, "file_id"))
	if err != nil {
		slog.Error("staticfile: get metadata", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if m == nil {
		httpx.Error(w, http.StatusNotFound, "could not find metadata by id")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, FileMetadata{
		FileID:        m.FileID,
		ContentType:   m.ContentType,
		IsUploaded:    m.IsUploaded,
		ExtensionData: m.ExtensionData,
		FileName:      m.FileName,
		OwnerID:       m.OwnerID,
		S3Key:         m.S3Key,
	})
}

func (d Deps) getPresignedURL(w http.ResponseWriter, r *http.Request) {
	if _, ok := callerOf(w, r); !ok {
		return
	}
	fileID := chi.URLParam(r, "file_id")
	m, err := d.Metadata.GetMetadata(r.Context(), fileID)
	if err != nil {
		slog.Error("staticfile: get metadata", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if m == nil {
		httpx.Error(w, http.StatusNotFound, "file not found")
		return
	}
	if !m.IsUploaded {
		httpx.Error(w, http.StatusNotFound, "file not yet uploaded")
		return
	}
	key := NewStaticFileKey(fileID).Key()
	url, err := d.Store.PresignGet(r.Context(), d.Bucket, key, getPresignTTL)
	if err != nil {
		slog.Error("staticfile: presign get", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "failed to get presigned URL")
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(url))
}

func (d Deps) putPresignedURL(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOf(w, r)
	if !ok {
		return
	}
	var req putFileRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	contentType := req.ContentType
	if contentType == "" {
		contentType = inferContentType(req.FileName)
		if contentType == "" {
			httpx.Error(w, http.StatusUnsupportedMediaType, "unknown or unsupported media type")
			return
		}
	}

	id := uuid.NewString()
	s3key := NewStaticFileKey(id).Key()
	permalink := strings.TrimSuffix(d.ServiceURL, "/") + "/" + s3key

	err := d.Metadata.PutMetadata(r.Context(), MetadataObject{
		FileID:        id,
		OwnerID:       caller.UserID,
		ContentType:   contentType,
		IsUploaded:    false,
		LastAccessed:  time.Now().UTC(),
		ExtensionData: rawJSON(req.ExtensionData),
		FileName:      req.FileName,
		S3Key:         s3key,
	})
	if err != nil {
		slog.Error("staticfile: put metadata", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	uploadURL, err := d.Store.PresignPut(r.Context(), d.Bucket, s3key, contentType, putPresignTTL)
	if err != nil {
		slog.Error("staticfile: presign put", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, putFileResponse{
		UploadURL:    uploadURL,
		FileLocation: permalink,
		ID:           id,
	})
}

func (d Deps) deleteFile(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOf(w, r)
	if !ok {
		return
	}
	fileID := chi.URLParam(r, "file_id")
	m, err := d.Metadata.GetMetadata(r.Context(), fileID)
	if err != nil {
		slog.Error("staticfile: get metadata", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if m == nil {
		httpx.Error(w, http.StatusNotFound, "not found")
		return
	}
	// Skip owner check for internal requests.
	if !caller.Internal && m.OwnerID != caller.UserID {
		slog.Warn("staticfile: delete requested by non-owner", "file_id", fileID)
		httpx.Error(w, http.StatusForbidden, "access denied")
		return
	}

	if err := d.Store.DeleteObject(r.Context(), d.Bucket, m.S3Key); err != nil {
		slog.Error("staticfile: delete object", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := d.Metadata.DeleteMetadata(r.Context(), fileID); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, http.StatusNotFound, "not found")
			return
		}
		slog.Error("staticfile: delete metadata", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Best-effort cleanup of scaled variants.
	if err := d.Store.DeleteByPrefix(r.Context(), d.Bucket,
		NewStaticFileKey(fileID).VariantPrefix()); err != nil {
		slog.Warn("staticfile: failed to delete scaled variants", "err", err)
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Ok"))
}

func (d Deps) bulkDeleteFile(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOf(w, r)
	if !ok {
		return
	}
	var req bulkDeleteRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if len(req.FileIDs) == 0 {
		httpx.ErrorJSON(w, http.StatusBadRequest, "Validation error: file_ids cannot be empty")
		return
	}
	if len(req.FileIDs) > maxBulkDelete {
		httpx.ErrorJSON(w, http.StatusBadRequest,
			"Validation error: Cannot delete more than 100 files at once")
		return
	}

	meta, err := d.Metadata.BulkGetMetadata(r.Context(), req.FileIDs)
	if err != nil {
		slog.Error("staticfile: bulk get metadata", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "Internal error")
		return
	}

	results := make([]deleteResult, 0, len(req.FileIDs))
	var s3Keys []string
	var toDelete []string

	for _, fileID := range req.FileIDs {
		m, found := meta[fileID]
		switch {
		case !found:
			results = append(results, deleteResult{FileID: fileID, Success: false,
				Error: strPtr("not found")})
		case !caller.Internal && m.OwnerID != caller.UserID:
			slog.Warn("staticfile: delete requested by non-owner", "file_id", fileID)
			results = append(results, deleteResult{FileID: fileID, Success: false,
				Error: strPtr("access denied")})
		default:
			s3Keys = append(s3Keys, m.S3Key)
			toDelete = append(toDelete, fileID)
			results = append(results, deleteResult{FileID: fileID, Success: true})
		}
	}

	if len(s3Keys) > 0 {
		s3Results := d.Store.DeleteObjects(r.Context(), d.Bucket, s3Keys)
		for idx, fileID := range toDelete {
			var derr error
			if idx < len(s3Results) {
				derr = s3Results[idx]
			} else {
				derr = errString("s3 deletion failed: no result")
			}
			if derr != nil {
				for i := range results {
					if results[i].FileID == fileID {
						slog.Error("staticfile: s3 delete failed", "file_id", fileID, "err", derr)
						results[i].Success = false
						results[i].Error = strPtr("s3 deletion failed: " + derr.Error())
					}
				}
			}
		}
	}

	// Metadata deletion only for files that passed S3 deletion.
	var succeeded []string
	for _, res := range results {
		if res.Success {
			succeeded = append(succeeded, res.FileID)
		}
	}
	if len(succeeded) > 0 {
		dbResults := d.Metadata.BulkDeleteMetadata(r.Context(), succeeded)
		for i, fileID := range succeeded {
			if i < len(dbResults) && dbResults[i] != nil {
				for j := range results {
					if results[j].FileID == fileID {
						slog.Error("staticfile: metadata delete failed",
							"file_id", fileID, "err", dbResults[i])
						results[j].Success = false
						results[j].Error = strPtr("metadata deletion failed: " + dbResults[i].Error())
					}
				}
			}
		}
		for _, fileID := range succeeded {
			if err := d.Store.DeleteByPrefix(r.Context(), d.Bucket,
				NewStaticFileKey(fileID).VariantPrefix()); err != nil {
				slog.Warn("staticfile: failed to delete scaled variants",
					"file_id", fileID, "err", err)
			}
		}
	}

	var okCount int
	for _, res := range results {
		if res.Success {
			okCount++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, bulkDeleteResponse{
		Total:     len(results),
		Succeeded: okCount,
		Failed:    len(results) - okCount,
		Results:   results,
	})
}

func strPtr(s string) *string { return &s }

type errString string

func (e errString) Error() string { return string(e) }

func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// inferContentType replaces FileType::from_str(ext).mime_type(). Covers the
// extension table via mime.TypeByExtension plus a few explicit entries for
// types Go's mime table doesn't know.
func inferContentType(fileName string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(fileName), "."))
	if ext == "" {
		return ""
	}
	switch ext {
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case "md", "markdown":
		return "text/markdown"
	case "heic":
		return "image/heic"
	case "avif":
		return "image/avif"
	}
	if ct := mime.TypeByExtension("." + ext); ct != "" {
		return strings.Split(ct, ";")[0]
	}
	// The Rust FileType table maps hundreds of code extensions to text/plain.
	return ""
}
