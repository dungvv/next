// Package convert ports services/convert_service. The Rust service ran
// LibreOffice inline (fork + lok bindings); the self-host port instead
// publishes a job envelope onto the JetStream "jobs" stream — the actual
// conversion runs in `macro worker` (internal/jobs). The backfill endpoint
// (re-zip DOCX BOM parts and enqueue conversions) is ported too.
package convert

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
	"github.com/macro-inc/macro/pkg/objectstore"
)

// JobEnvelopeType is the envelope `type` for queued conversions.
const JobEnvelopeType = "convert.request"

// JobsSubject is the subject inside the `jobs` stream carrying convert jobs.
const JobsSubject = "jobs.convert"

// ConvertRequest mirrors model::convert::ConvertRequest.
type ConvertRequest struct {
	FromBucket string `json:"from_bucket"`
	ToBucket   string `json:"to_bucket"`
	FromKey    string `json:"from_key"`
	ToKey      string `json:"to_key"`
}

// QueueMessage mirrors model::convert::ConvertQueueMessage.
type QueueMessage struct {
	JobID      string `json:"job_id"`
	FromBucket string `json:"from_bucket"`
	ToBucket   string `json:"to_bucket"`
	FromKey    string `json:"from_key"`
	ToKey      string `json:"to_key"`
}

// Deps for the convert router.
type Deps struct {
	JS        jetstream.JetStream // may be nil when NATS is unavailable
	Store     objectstore.Store   // used by backfill
	DB        docxQuerier         // macrodb, used by backfill; may be nil
	DocBucket string              // DOCUMENT_STORAGE_BUCKET
}

// docxQuerier is the macrodb read port for the backfill endpoint.
type docxQuerier interface {
	DocxFiles(ctx context.Context, limit, offset int64) ([]docxDocument, int64, error)
}

// Publisher publishes a convert job envelope. JetStream-backed.
type Publisher struct {
	JS jetstream.JetStream
}

// EnsureStream provisions the jobs stream for convert subjects.
func EnsureStream(ctx context.Context, js jetstream.JetStream) error {
	_, err := natsx.EnsureStream(ctx, js, events.StreamJobs, []string{"jobs.>"})
	return err
}

// Publish enqueues a conversion job. Returns the job id.
func (p Publisher) Publish(ctx context.Context, req ConvertRequest) (string, error) {
	jobID, err := uuid.NewV7()
	if err != nil {
		jobID = uuid.New() // fall back to v4 if v7 unavailable
	}
	msg := QueueMessage{
		JobID:      jobID.String(),
		FromBucket: req.FromBucket,
		ToBucket:   req.ToBucket,
		FromKey:    req.FromKey,
		ToKey:      req.ToKey,
	}
	env, err := events.New(JobEnvelopeType, "convert", msg.JobID, 1, msg)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	m := nats.NewMsg(JobsSubject)
	m.Header.Set("Nats-Msg-Id", msg.JobID)
	m.Data = payload
	if _, err := p.JS.PublishMsg(ctx, m); err != nil {
		return "", fmt.Errorf("publish convert job: %w", err)
	}
	return msg.JobID, nil
}

// Register installs the /internal subtree routes relative to r:
// POST {r}/convert and POST {r}/backfill/docx. The caller mounts these under
// /internal and /convert/internal (mirroring the Rust /internal nest and the
// /convert gateway prefix).
func (d Deps) Register(r chi.Router) {
	r.Post("/convert", d.convertHandler)
	r.Post("/backfill/docx", d.backfillDocxHandler)
}

// convertHandler ports api/convert.rs::handler, minus the in-process
// LibreOffice fork: validate the conversion pair, publish the job, return
// the job id.
func (d Deps) convertHandler(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok || !auth.RequireInternal(w, caller) {
		return
	}
	if d.JS == nil {
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	var req ConvertRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	fromType, ok := fileTypeFromKey(req.FromKey)
	if !ok {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get file type")
		return
	}
	toType, ok := fileTypeFromKey(req.ToKey)
	if !ok {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get file type")
		return
	}
	if _, err := lokFilter(fromType, toType); err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	jobID, err := Publisher{JS: d.JS}.Publish(r.Context(), req)
	if err != nil {
		slog.Error("convert: publish job", "err", err)
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to enqueue conversion")
		return
	}
	slog.Info("convert: enqueued job", "job_id", jobID)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"job_id": jobID})
}

// fileTypeFromKey mirrors `key.split('.').next_back() |> FileType::from_str`,
// restricted to the types the LOK filter table supports.
func fileTypeFromKey(key string) (string, bool) {
	ext := key
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		ext = key[i+1:]
	}
	switch strings.ToLower(ext) {
	case "docx":
		return "docx", true
	case "pdf":
		return "pdf", true
	case "xlsx":
		return "xlsx", true
	case "pptx":
		return "pptx", true
	case "html":
		return "html", true
	}
	return "", false
}

// lokFilter ports utils.rs::get_lok_filter_from_file_types.
func lokFilter(from, to string) (string, error) {
	switch from {
	case "docx":
		if to == "pdf" {
			return "writer_pdf_Export", nil
		}
	case "xlsx":
		if to == "html" {
			return "calc_HTML_WebQuery", nil
		}
	case "pptx":
		if to == "pdf" {
			return "impress_pdf_Export", nil
		}
	default:
		return "", fmt.Errorf("unsupported conversion of %s for conversion", from)
	}
	return "", fmt.Errorf("unsupported conversion of %s to %s", from, to)
}

// isDocxKey mirrors process/convert.rs::is_docx_key.
func isDocxKey(key string) bool {
	return strings.HasSuffix(strings.ToLower(key), ".docx")
}
