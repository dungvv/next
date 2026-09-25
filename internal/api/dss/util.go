package dss

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// writeTypedErr mirrors the dss GenericResponse error envelope.
func writeErr(w http.ResponseWriter, status int, msg string) {
	httpx.WriteJSON(w, status, GenericResponse{Error: true, Message: msg})
}

func writeOK(w http.ResponseWriter, data any) {
	httpx.WriteJSON(w, http.StatusOK, GenericResponse{Error: false, Data: mustJSON(data)})
}

// callerOr401 extracts the caller or writes 401. Returns ok=false when
// unauthenticated.
func callerOr401(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	c, ok := callerFrom(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return Caller{}, false
	}
	return c, true
}

func queryInt(r *http.Request, name string, def int64) int64 {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// mapDocumentMetadata converts the sqlc full-document row into the
// model::document::DocumentMetadata JSON shape.
func mapDocumentMetadata(row macrodb.GetDocument2Row) DocumentMetadata {
	var subType *string
	if row.SubType.Valid {
		s := string(row.SubType.DocumentSubTypeValue)
		subType = &s
	}
	var projectName *string
	if row.ProjectName != "" {
		projectName = &row.ProjectName
	}
	var sha *string
	if row.Sha != "" {
		sha = &row.Sha
	}
	return DocumentMetadata{
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
	}
}

// docResponseMetadata maps DocumentMetadata to DocumentResponseMetadata
// (same fields minus projectId/projectName/deletedAt).
func docResponseMetadata(m DocumentMetadata) DocumentResponseMetadata {
	return DocumentResponseMetadata{
		DocumentID:            m.DocumentID,
		DocumentVersionID:     m.DocumentVersionID,
		Owner:                 m.Owner,
		DocumentName:          m.DocumentName,
		FileType:              m.FileType,
		Sha:                   m.Sha,
		BranchedFromID:        m.BranchedFromID,
		BranchedFromVersionID: m.BranchedFromVersionID,
		DocumentFamilyID:      m.DocumentFamilyID,
		DocumentBom:           m.DocumentBom,
		ModificationData:      m.ModificationData,
		CreatedAt:             m.CreatedAt,
		UpdatedAt:             m.UpdatedAt,
		SubType:               m.SubType,
	}
}

func pgText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func pgTextStr(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

func pgTS(t *time.Time) pgtype.Timestamp {
	if t == nil {
		return pgtype.Timestamp{}
	}
	return pgtype.Timestamp{Time: *t, Valid: true}
}

// isNoRows reports whether err is pgx.ErrNoRows.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// chiParam mirrors axum Path extraction.
func chiParam(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}

// validBomPath reports whether a caller-supplied BOM part path is a safe
// zip-internal name. Paths are never presigned (Rust presigns the sha, not
// the path), but they are persisted and echoed back in location responses —
// reject absolute paths, traversal segments, backslashes, and control
// characters so a stored BOM can never carry a hostile "key".
func validBomPath(p string) bool {
	if p == "" || len(p) > 1024 {
		return false
	}
	if p[0] == '/' || strings.Contains(p, "\\") || strings.ContainsRune(p, '\x00') {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// validBomSHA reports whether sha is a safe object key for a
// content-addressed docx BOM part. Rust stores docx parts in S3 keyed by
// their sha alone (not under a document prefix), so the value must be a
// plain filename-safe token: no separators, no traversal, no leading dot,
// bounded length. Anything else is rejected rather than presigned.
func validBomSHA(sha string) bool {
	if sha == "" || len(sha) > 256 {
		return false
	}
	if sha[0] == '.' || strings.Contains(sha, "..") {
		return false
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		ok := c >= '0' && c <= '9' ||
			c >= 'a' && c <= 'z' ||
			c >= 'A' && c <= 'Z' ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}
