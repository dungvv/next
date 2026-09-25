package jobs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/macro-inc/macro/pkg/events"
)

// docxUnzipJob is the envelope payload for subjDocxUnzip. It replaces the
// S3 bucket-notification event that triggered the Rust lambda; producers
// (upload pipeline / MinIO notification relay) publish one message per object.
type docxUnzipJob struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// convertQueueMessage mirrors model::convert::ConvertQueueMessage (snake_case
// JSON — the wire contract of the old SQS queue).
type convertQueueMessage struct {
	JobID      string `json:"job_id"`
	FromBucket string `json:"from_bucket"`
	ToBucket   string `json:"to_bucket"`
	FromKey    string `json:"from_key"`
	ToKey      string `json:"to_key"`
}

// docxUploadJobResult mirrors models::DocxUploadJobResult (camelCase JSON)
// pushed to the user's websocket connections on job failure.
type docxUploadJobResult struct {
	JobID   string `json:"jobId"`
	Status  string `json:"status"`
	JobType string `json:"jobType"`
	Data    struct {
		Error   bool    `json:"error"`
		Data    any     `json:"data,omitempty"`
		Message *string `json:"message,omitempty"`
	} `json:"data"`
}

// documentKeyParts parses the staging key
// `{owner}/{document_id}/{document_bom_id}.docx`.
type documentKeyParts struct {
	Owner         string // principal string, e.g. "macro|user@x.com"
	DocumentID    string
	DocumentBomID int64
}

func parseDocumentKey(key string) (documentKeyParts, error) {
	split := strings.Split(key, "/")
	if len(split) != 3 {
		return documentKeyParts{}, fmt.Errorf("invalid key format: %q", key)
	}
	// S3 event notifications deliver keys form-encoded; PathUnescape matches
	// Rust's urlencoding::decode (no '+' -> space conversion).
	owner, err := url.PathUnescape(split[0])
	if err != nil {
		return documentKeyParts{}, fmt.Errorf("decode owner segment: %w", err)
	}
	stem, _, _ := strings.Cut(split[2], ".")
	bomID, err := strconv.ParseInt(stem, 10, 64)
	if err != nil {
		return documentKeyParts{}, fmt.Errorf("parse bom id %q: %w", stem, err)
	}
	return documentKeyParts{Owner: owner, DocumentID: split[1], DocumentBomID: bomID}, nil
}

// toKey rebuilds build_docx_staging_bucket_document_key: {owner}/{doc}/{bom}.docx
func (p documentKeyParts) toKey() string {
	return fmt.Sprintf("%s/%s/%d.docx", p.Owner, p.DocumentID, p.DocumentBomID)
}

// convertedPDFKey is build_docx_to_pdf_converted_document_key.
func (p documentKeyParts) convertedPDFKey() string {
	return fmt.Sprintf("%s/%s/converted.pdf", p.Owner, p.DocumentID)
}

type documentBomPart struct {
	SHA     string
	Path    string
	Content []byte
}

// handleDocxUnzip ports services/docx_unzip_handler.
func (d *deps) handleDocxUnzip(ctx context.Context, env events.Envelope) error {
	var job docxUnzipJob
	if err := json.Unmarshal(env.Data, &job); err != nil {
		return fmt.Errorf("decode docx_unzip job: %w", err)
	}
	bucket := job.Bucket
	if bucket == "" {
		bucket = d.jcfg.DocxStagingBucket
	}
	if job.Key == "" {
		return errors.New("docx_unzip job missing key")
	}

	parts, err := parseDocumentKey(job.Key)
	if err != nil {
		return err
	}
	log := slog.With("document_id", parts.DocumentID, "document_bom_id", parts.DocumentBomID)

	if err := d.processDocx(ctx, bucket, parts, log); err != nil {
		log.Error("docx unzip failed", "err", err)
		// Mirror handle_docx_unzip_failure: notify the owner's websockets.
		if nerr := d.notifyDocxUploadFailure(ctx, parts); nerr != nil {
			log.Error("docx unzip failure notification failed", "err", nerr)
		}
		return err
	}
	return nil
}

func (d *deps) processDocx(ctx context.Context, bucket string, parts documentKeyParts, log *slog.Logger) error {
	// Idempotency: committed BomPart rows are the done marker for this bom
	// id (every later write happens after the convert enqueue, so their
	// presence means a previous delivery ran to completion — or at least
	// past the point where re-running would duplicate rows and sha counts).
	// A redelivery then acks without re-enqueuing convert or re-saving.
	committed, err := d.bomPartsCommitted(ctx, parts.DocumentBomID)
	if err != nil {
		return fmt.Errorf("check existing bom parts: %w", err)
	}
	if committed {
		log.Info("docx_unzip: bom parts already committed, skipping redelivery")
		return nil
	}

	jobID, _, err := d.jobForDocxUpload(ctx, parts.DocumentID)
	if err != nil {
		return fmt.Errorf("get docx upload job: %w", err)
	}
	if jobID == "" {
		jobID = uuid.NewString()
		log.Warn("no job id found, generated one")
	}

	// Queue the docx->pdf conversion (was: SQS convert queue). The msg id
	// dedupes a racing/concurrent re-publish within the stream's duplicate
	// window.
	if err := d.publishJobID(ctx, subjConvert, "document.convert", parts.DocumentID,
		fmt.Sprintf("docx_unzip/convert/%s/%d", parts.DocumentID, parts.DocumentBomID),
		convertQueueMessage{
			JobID:      jobID,
			FromBucket: bucket,
			FromKey:    parts.toKey(),
			ToBucket:   d.jcfg.DocumentStorageBucket,
			ToKey:      parts.convertedPDFKey(),
		}); err != nil {
		return fmt.Errorf("enqueue convert message: %w", err)
	}

	obj, err := d.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(parts.toKey()),
	})
	if err != nil {
		return fmt.Errorf("get document %s/%s: %w", bucket, parts.toKey(), err)
	}
	documentData, err := io.ReadAll(obj.Body)
	obj.Body.Close()
	if err != nil {
		return fmt.Errorf("read document body: %w", err)
	}

	if _, err := d.pools.MacroDB.Exec(ctx, `
		UPDATE "Document"
		SET "uploaded" = true,
		    "contentState" = CASE
		        WHEN "contentState" = 'ready' AND "contentLocation" = 'converted_pdf'
		            THEN 'ready'
		        ELSE 'pending'
		    END,
		    "contentLocation" = 'converted_pdf'
		WHERE id = $1
	`, parts.DocumentID); err != nil {
		return fmt.Errorf("update uploaded status: %w", err)
	}

	bomParts, err := unzipDocx(documentData)
	if err != nil {
		return fmt.Errorf("unzip docx: %w", err)
	}

	shas := make([]string, len(bomParts))
	for i, bp := range bomParts {
		shas[i] = bp.SHA
	}

	toUpload, err := d.findNonExistingSHAs(ctx, shas)
	if err != nil {
		return fmt.Errorf("find non-existing shas: %w", err)
	}

	if len(toUpload) > 0 {
		uploadSet := make(map[string]struct{}, len(toUpload))
		for _, s := range toUpload {
			uploadSet[s] = struct{}{}
		}
		g, gctx := errgroup.WithContext(ctx)
		for _, bp := range bomParts {
			if _, ok := uploadSet[bp.SHA]; !ok {
				continue
			}
			g.Go(func() error {
				_, err := d.s3.PutObject(gctx, &s3.PutObjectInput{
					Bucket: aws.String(d.jcfg.DocumentStorageBucket),
					Key:    aws.String(bp.SHA),
					Body:   bytes.NewReader(bp.Content),
				})
				if err != nil {
					return fmt.Errorf("upload bom part %s: %w", bp.SHA, err)
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}

	inserted, err := d.saveBomPartsOnce(ctx, bomParts, parts.DocumentBomID)
	if err != nil {
		return fmt.Errorf("save bom parts: %w", err)
	}
	if !inserted {
		// A concurrent delivery of this same message already committed the
		// rows — do not double-increment the sha refcounts.
		log.Info("docx_unzip: bom parts already saved by concurrent delivery")
		return nil
	}

	if err := d.incrementSHACounts(ctx, shas); err != nil {
		return fmt.Errorf("increment sha counts: %w", err)
	}

	log.Info("docx unzip complete", "bom_parts", len(bomParts))
	return nil
}

// bomPartsCommitted reports whether BomPart rows already exist for this
// document bom id — the "already processed" marker for redeliveries.
func (d *deps) bomPartsCommitted(ctx context.Context, bomID int64) (bool, error) {
	var n int64
	err := d.pools.MacroDB.QueryRow(ctx, `
		SELECT COUNT(*) FROM "BomPart" WHERE "documentBomId" = $1
	`, bomID).Scan(&n)
	return n > 0, err
}

// jobForDocxUpload mirrors macro_db_client::docx_unzip::get_job_for_docx_upload.
func (d *deps) jobForDocxUpload(ctx context.Context, documentID string) (jobID, jobType string, err error) {
	err = d.pools.MacroDB.QueryRow(ctx, `
		SELECT "jobId", "jobType" FROM "UploadJob" WHERE "documentId" = $1
	`, documentID).Scan(&jobID, &jobType)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return jobID, jobType, err
}

// saveBomPartsOnce ports macro_db_client::docx_unzip::save_bom_parts_to_db
// with idempotency for at-least-once delivery: it takes a transaction-scoped
// advisory lock keyed on the bom id so two deliveries racing the insert
// serialize, then re-checks inside the lock. Returns false when the rows
// already existed (nothing inserted — caller must not bump sha counts).
func (d *deps) saveBomPartsOnce(ctx context.Context, parts []documentBomPart, bomID int64) (bool, error) {
	if len(parts) == 0 {
		return true, nil
	}
	tx, err := d.pools.MacroDB.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
	`, "docx_unzip:"+strconv.FormatInt(bomID, 10)); err != nil {
		return false, fmt.Errorf("acquire bom lock: %w", err)
	}

	var existing int64
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM "BomPart" WHERE "documentBomId" = $1
	`, bomID).Scan(&existing); err != nil {
		return false, fmt.Errorf("count existing bom parts: %w", err)
	}
	if existing > 0 {
		return false, tx.Commit(ctx)
	}

	var sb strings.Builder
	sb.WriteString(`INSERT INTO "BomPart" ("documentBomId", "sha", "path") VALUES `)
	args := make([]any, 0, 1+2*len(parts))
	args = append(args, bomID)
	for i, bp := range parts {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "($1, $%d, $%d)", len(args)+1, len(args)+2)
		args = append(args, bp.SHA, bp.Path)
	}
	if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
		return false, fmt.Errorf("insert bom parts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit bom parts: %w", err)
	}
	return true, nil
}

// unzipDocx ports service::document::unzip: every zip member becomes a bom
// part keyed by the hex sha256 of its content.
func unzipDocx(documentContent []byte) ([]documentBomPart, error) {
	zr, err := zip.NewReader(bytes.NewReader(documentContent), int64(len(documentContent)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	parts := make([]documentBomPart, 0, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip member %q: %w", f.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read zip member %q: %w", f.Name, err)
		}
		sum := sha256.Sum256(content)
		parts = append(parts, documentBomPart{
			SHA:     hex.EncodeToString(sum[:]),
			Path:    f.Name,
			Content: content,
		})
	}
	return parts, nil
}

// findNonExistingSHAs ports find_non_existing_shas_string.
func (d *deps) findNonExistingSHAs(ctx context.Context, shas []string) ([]string, error) {
	var missing []string
	for _, sha := range shas {
		n, err := d.redis.Exists(ctx, shaCountKeyPrefix+sha).Result()
		if err != nil {
			return nil, fmt.Errorf("check sha %s: %w", sha, err)
		}
		if n == 0 {
			missing = append(missing, sha)
		}
	}
	return missing, nil
}

// incrementSHACounts ports macro_sha_count_client::increment_counts: INCR the
// count key and drop the sha from the delete bucket.
func (d *deps) incrementSHACounts(ctx context.Context, shas []string) error {
	for _, sha := range shas {
		if err := d.redis.Incr(ctx, shaCountKeyPrefix+sha).Err(); err != nil {
			return fmt.Errorf("incr sha %s: %w", sha, err)
		}
		if err := d.redis.SRem(ctx, shaDeleteBucket, sha).Err(); err != nil {
			return fmt.Errorf("remove sha %s from delete bucket: %w", sha, err)
		}
	}
	return nil
}

// notifyDocxUploadFailure ports handle_docx_unzip_failure: instead of
// invoking the websocket-response Lambda, it publishes the job result on
// realtime.user.<owner> for the gateway to fan out. Only user owners have
// websocket connections; bot/team owners are skipped.
func (d *deps) notifyDocxUploadFailure(ctx context.Context, parts documentKeyParts) error {
	jobID, jobType, err := d.jobForDocxUpload(ctx, parts.DocumentID)
	if err != nil {
		return fmt.Errorf("get docx upload job: %w", err)
	}
	if jobID == "" {
		return nil
	}
	if !strings.HasPrefix(parts.Owner, "macro|") {
		slog.Info("docx unzip failure: non-user owner, skipping realtime notify",
			"owner", parts.Owner, "document_id", parts.DocumentID)
		return nil
	}
	msg := "error unzipping document"
	var result docxUploadJobResult
	result.JobID = jobID
	result.Status = "Failed"
	result.JobType = jobType
	result.Data.Error = true
	result.Data.Message = &msg
	return d.publishRealtime(parts.Owner, "docx_upload.result", result)
}
