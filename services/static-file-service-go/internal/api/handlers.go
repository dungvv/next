// Package api mirrors api/: the file router (presigned PUT/GET, metadata,
// delete, bulk-delete), the health route, and the swagger surface.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dungvv/next/services/static-file-service-go/internal/authz"
	"github.com/dungvv/next/services/static-file-service-go/internal/dynamodb"
	"github.com/dungvv/next/services/static-file-service-go/internal/filetype"
	"github.com/dungvv/next/services/static-file-service-go/internal/s3key"
)

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Message: message})
}

// PutPresignedURL mirrors put_presigned_url::put_presigned_url (PUT /api/file).
func (s *Server) PutPresignedURL(w http.ResponseWriter, r *http.Request) {
	user := authz.FromContext(r.Context())

	var req PutFileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestSize)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	contentType := ""
	if req.ContentType != nil && *req.ContentType != "" {
		contentType = *req.ContentType
	} else {
		ext := req.FileName
		if i := strings.LastIndexByte(ext, '.'); i >= 0 {
			ext = ext[i+1:]
		}
		contentType = filetype.MIMEType(ext)
		if contentType == "" {
			writeError(w, http.StatusUnsupportedMediaType, "unknown or unsupported media type")
			return
		}
	}

	id := uuid.NewString()
	key := s3key.New(id)
	s3KeyStr := key.String()
	permalink := s.Config.StaticFileServiceURL + "/" + s3KeyStr

	metadata := dynamodb.MetadataObject{
		FileID:        id,
		ContentType:   contentType,
		IsUploaded:    false,
		LastAccessed:  time.Now().UTC(),
		OwnerID:       user.UserID,
		ExtensionData: req.ExtensionData,
		FileName:      req.FileName,
		S3Key:         s3KeyStr,
	}
	if err := s.Metadata.PutMetadata(r.Context(), metadata); err != nil {
		slog.Error("could not create metadata", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	uploadURL, err := s.Storage.PutPresignedURL(r.Context(), s3KeyStr, contentType)
	if err != nil {
		slog.Error("could not create presigned url", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(PutFileResponse{
		ID:           id,
		UploadURL:    uploadURL,
		FileLocation: permalink,
	})
}

// GetPresignedURL mirrors get_file::handle_get_presigned_url
// (GET /api/file/{file_id}/presigned-url).
func (s *Server) GetPresignedURL(w http.ResponseWriter, r *http.Request) {
	fileID := chi.URLParam(r, "file_id")

	metadata, err := s.Metadata.GetMetadata(r.Context(), fileID)
	if err != nil {
		slog.Error("error getting metadata", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if metadata == nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if !metadata.IsUploaded {
		writeError(w, http.StatusNotFound, "file not yet uploaded")
		return
	}

	presignedURL, err := s.Storage.GetPresignedURL(r.Context(), s3key.New(fileID).String())
	if err != nil {
		slog.Error("error getting presigned URL from S3", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get presigned URL")
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(presignedURL))
}

// GetMetadata mirrors metadata::handle_get_metadata
// (GET /api/file/metadata/{file_id}).
func (s *Server) GetMetadata(w http.ResponseWriter, r *http.Request) {
	fileID := chi.URLParam(r, "file_id")

	metadata, err := s.Metadata.GetMetadata(r.Context(), fileID)
	if err != nil {
		slog.Error("error getting metadata", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if metadata == nil {
		writeError(w, http.StatusNotFound, "could not find metadata by id")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(FileMetadata{
		FileID:        metadata.FileID,
		ContentType:   metadata.ContentType,
		IsUploaded:    metadata.IsUploaded,
		ExtensionData: metadata.ExtensionData,
		FileName:      metadata.FileName,
		OwnerID:       metadata.OwnerID,
		S3Key:         metadata.S3Key,
	})
}

// DeleteFile mirrors delete_file::handle_delete_file
// (DELETE /api/file/{file_id}).
func (s *Server) DeleteFile(w http.ResponseWriter, r *http.Request) {
	user := authz.FromContext(r.Context())
	fileID := chi.URLParam(r, "file_id")

	metadata, err := s.Metadata.GetMetadata(r.Context(), fileID)
	if err != nil {
		slog.Error("failed to delete file", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if metadata == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// Skip owner check for internal requests.
	if user.Caller != authz.CallerInternal && metadata.OwnerID != user.UserID {
		slog.Warn("delete requested by non-owner")
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	if err := s.Storage.HardDeleteObject(r.Context(), metadata.S3Key); err != nil {
		slog.Error("failed to delete s3 object", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if err := s.Metadata.DeleteMetadata(r.Context(), fileID); err != nil {
		var nf *dynamodb.NotFoundError
		if errors.As(err, &nf) {
			slog.Warn("metadata not found", "error", nf.Message)
			writeError(w, http.StatusNotFound, "not found")
		} else {
			slog.Error("error deleting metadata", "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	// Best-effort cleanup of transformed variants.
	if err := s.Storage.DeleteObjectsByPrefix(r.Context(), s3key.New(fileID).VariantPrefix()); err != nil {
		slog.Warn("failed to delete scaled variants", "error", err)
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Ok"))
}

// BulkDeleteFile mirrors bulk_delete_file::handle_bulk_delete_file
// (POST /api/file/bulk-delete).
func (s *Server) BulkDeleteFile(w http.ResponseWriter, r *http.Request) {
	user := authz.FromContext(r.Context())

	var req BulkDeleteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestSize)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.FileIDs) == 0 {
		writeError(w, http.StatusBadRequest, "Validation error: file_ids cannot be empty")
		return
	}
	if len(req.FileIDs) > maxFileIDs {
		writeError(w, http.StatusBadRequest,
			"Validation error: Cannot delete more than "+itoa(maxFileIDs)+" files at once")
		return
	}

	metadataByID, err := s.Metadata.BulkGetMetadata(r.Context(), req.FileIDs)
	if err != nil {
		slog.Error("failed to fetch metadata", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal error")
		return
	}

	results := make([]DeleteResult, 0, len(req.FileIDs))
	var s3KeysToDelete, fileIDsToDelete []string
	isInternal := user.Caller == authz.CallerInternal

	for _, fileID := range req.FileIDs {
		metadata, ok := metadataByID[fileID]
		if !ok {
			msg := "not found"
			results = append(results, DeleteResult{FileID: fileID, Success: false, Error: &msg})
			continue
		}
		if !isInternal && metadata.OwnerID != user.UserID {
			slog.Warn("delete requested by non-owner", "file_id", fileID)
			msg := "access denied"
			results = append(results, DeleteResult{FileID: fileID, Success: false, Error: &msg})
			continue
		}
		s3KeysToDelete = append(s3KeysToDelete, metadata.S3Key)
		fileIDsToDelete = append(fileIDsToDelete, fileID)
		results = append(results, DeleteResult{FileID: fileID, Success: true})
	}

	// Bulk S3 deletion; align per-key outcomes back onto results.
	if len(s3KeysToDelete) > 0 {
		s3Results := s.Storage.BulkHardDeleteObjects(r.Context(), s3KeysToDelete)
		for i, fileID := range fileIDsToDelete {
			for j := range results {
				if results[j].FileID != fileID {
					continue
				}
				switch {
				case i < len(s3Results) && s3Results[i] == nil:
					// S3 deletion succeeded, keep success: true.
				case i < len(s3Results):
					slog.Error("failed to delete from S3", "file_id", fileID, "error", s3Results[i])
					results[j].Success = false
					msg := "s3 deletion failed: " + s3Results[i].Error()
					results[j].Error = &msg
				default:
					results[j].Success = false
					msg := "s3 deletion failed: no result"
					results[j].Error = &msg
				}
			}
		}
	}

	// Bulk metadata deletion for files that passed S3 deletion.
	var succeeded []int
	for i := range results {
		if results[i].Success {
			succeeded = append(succeeded, i)
		}
	}
	if len(succeeded) > 0 {
		ids := make([]string, 0, len(succeeded))
		for _, i := range succeeded {
			ids = append(ids, results[i].FileID)
		}
		dbResults := s.Metadata.BulkDeleteMetadata(r.Context(), ids)
		for k, dbErr := range dbResults {
			if dbErr == nil {
				continue
			}
			i := succeeded[k]
			slog.Error("failed to delete metadata", "file_id", results[i].FileID, "error", dbErr)
			results[i].Success = false
			msg := "metadata deletion failed: " + dbErr.Error()
			results[i].Error = &msg
		}
		for _, i := range succeeded {
			if err := s.Storage.DeleteObjectsByPrefix(r.Context(), s3key.New(results[i].FileID).VariantPrefix()); err != nil {
				slog.Warn("failed to delete scaled variants", "file_id", results[i].FileID, "error", err)
			}
		}
	}

	succeededCount := 0
	for _, res := range results {
		if res.Success {
			succeededCount++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(BulkDeleteResponse{
		Total:     len(results),
		Succeeded: succeededCount,
		Failed:    len(results) - succeededCount,
		Results:   results,
	})
}

// Health mirrors health::health_handler (GET /api/health).
func (s *Server) Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("healthy"))
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
