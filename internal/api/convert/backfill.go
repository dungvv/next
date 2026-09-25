package convert

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// backfillDocxHandler ports api/backfill/backfill_docx.rs: pages over all
// docx documents, re-zips their BOM parts into a temp docx in the document
// bucket, and enqueues a convert job for each missing converted.pdf.
func (d Deps) backfillDocxHandler(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok || !auth.RequireInternal(w, caller) {
		return
	}
	if d.DB == nil || d.Store == nil || d.JS == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "backfill dependencies unavailable")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
	if limit <= 0 {
		limit = 10
	}
	offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)

	documents, totalCount, err := d.DB.DocxFiles(r.Context(), limit, offset)
	if err != nil {
		slog.Error("convert backfill: unable to get documents", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get documents")
		return
	}

	pub := Publisher{JS: d.JS}
	const chunkSize = 25
	for start := 0; start < len(documents); start += chunkSize {
		end := min(start+chunkSize, len(documents))
		slog.Info("convert backfill: processing chunk",
			"chunk", start/chunkSize, "total", len(documents)/chunkSize)
		for _, doc := range documents[start:end] {
			req, ok, err := d.processDocx(r.Context(), doc)
			if err != nil {
				slog.Error("convert backfill: unable to process document",
					"document_id", doc.DocumentID, "err", err)
				continue
			}
			if !ok {
				continue
			}
			if _, err := pub.Publish(r.Context(), *req); err != nil {
				slog.Error("convert backfill: unable to enqueue message", "err", err)
			}
		}
	}

	nextOffset := int64(0)
	if offset+limit < totalCount {
		nextOffset = offset + limit
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strconv.FormatInt(nextOffset, 10)))
}

// convertedDocumentKey builds `{owner}/{document_id}/converted.pdf`
// (s3_key::build_docx_to_pdf_converted_document_key).
func convertedDocumentKey(owner, documentID string) string {
	return owner + "/" + documentID + "/converted.pdf"
}

// tempDocxKey builds `temp_files/{document_id}.docx`
// (s3_key::build_temp_docx_key).
func tempDocxKey(documentID string) string {
	return "temp_files/" + documentID + ".docx"
}

// processDocx ports process_docx: skip when the converted pdf already exists
// or BOM parts are missing; otherwise fetch parts, zip into a temp docx,
// upload, and return the convert request to enqueue.
func (d Deps) processDocx(ctx context.Context, doc docxDocument) (*ConvertRequest, bool, error) {
	if len(doc.BomParts) == 0 {
		slog.Warn("convert backfill: unable to get bom parts", "document_id", doc.DocumentID)
		return nil, false, nil
	}
	bucket := d.DocBucket
	toKey := convertedDocumentKey(doc.Owner, doc.DocumentID)

	exists, err := d.Store.Exists(ctx, bucket, toKey)
	if err != nil {
		return nil, false, fmt.Errorf("unable to check if converted file exists: %w", err)
	}
	if exists {
		slog.Info("convert backfill: converted file already exists", "document_id", doc.DocumentID)
		return nil, false, nil
	}

	contents := make(map[string][]byte, len(doc.BomParts))
	for _, part := range doc.BomParts {
		obj, err := d.Store.GetObject(ctx, bucket, part.SHA)
		if err != nil {
			slog.Error("convert backfill: unable to get sha",
				"document_id", doc.DocumentID, "sha", part.SHA, "err", err)
			return nil, false, nil
		}
		data, err := io.ReadAll(obj.Body)
		obj.Body.Close()
		if err != nil {
			return nil, false, fmt.Errorf("read bom part %s: %w", part.SHA, err)
		}
		contents[part.SHA] = data
	}

	zipped, err := zipBomParts(doc.BomParts, contents)
	if err != nil {
		return nil, false, err
	}

	fromKey := tempDocxKey(doc.DocumentID)
	if err := d.Store.PutObject(ctx, bucket, fromKey, bytes.NewReader(zipped), ""); err != nil {
		return nil, false, fmt.Errorf("unable to put converted file: %w", err)
	}
	slog.Info("convert backfill: converted file created", "document_id", doc.DocumentID)

	return &ConvertRequest{
		FromBucket: bucket,
		ToBucket:   bucket,
		FromKey:    fromKey,
		ToKey:      toKey,
	}, true, nil
}

// zipBomParts ports zip_bom_parts: write each part's content at its path.
func zipBomParts(parts []bomPart, contents map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, part := range parts {
		content, ok := contents[part.SHA]
		if !ok {
			return nil, fmt.Errorf("missing content for sha %s", part.SHA)
		}
		f, err := zw.Create(part.Path)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(content); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
