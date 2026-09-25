package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/macro-inc/macro/pkg/events"
)

// convertTimeout mirrors MAX_WAIT_TIME_SECONDS in convert_service's
// process/convert.rs.
const convertTimeout = 30 * time.Second

// handleConvert ports convert_service's process/convert.rs::process_message:
// download the source object, convert it with LibreOffice, and upload the
// result. For docx staging keys it then notifies the owner's websockets with
// the DocxUploadJobResult (replacing the WEB_SOCKET_RESPONSE_LAMBDA invoke).
//
// The Rust worker forked and drove LibreOfficeKit in-process (LOK_PATH); the
// Go port shells out to `soffice --headless --convert-to <to>:<filter>`, so
// the worker image must ship LibreOffice (set SOFFICE_PATH otherwise).
func (d *deps) handleConvert(ctx context.Context, env events.Envelope) error {
	var req convertQueueMessage
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return fmt.Errorf("decode convert job: %w", err)
	}
	if req.JobID == "" || req.FromBucket == "" || req.FromKey == "" ||
		req.ToBucket == "" || req.ToKey == "" {
		return errors.New("convert job missing required fields")
	}
	log := slog.With("job_id", req.JobID, "from_key", req.FromKey, "to_key", req.ToKey)

	err := d.runConvert(ctx, req, log)

	// Only docx conversions feed the docx-upload websocket result.
	if isDocxKey(req.FromKey) {
		if nerr := d.notifyConvertResult(ctx, req, err == nil); nerr != nil {
			log.Error("convert result notification failed", "err", nerr)
			if err == nil {
				return nerr
			}
		}
	}
	return err
}

// runConvert performs the download -> soffice -> upload pipeline.
func (d *deps) runConvert(ctx context.Context, req convertQueueMessage, log *slog.Logger) error {
	fromType, ok := fileTypeFromKey(req.FromKey)
	if !ok {
		return fmt.Errorf("unable to get file type from %q", req.FromKey)
	}
	toType, ok := fileTypeFromKey(req.ToKey)
	if !ok {
		return fmt.Errorf("unable to get file type from %q", req.ToKey)
	}
	filter, err := lokFilter(fromType, toType)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "convert-"+req.JobID)
	if err != nil {
		return fmt.Errorf("create job dir: %w", err)
	}
	// cleanup_folder in the Rust worker.
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Error("unable to cleanup folder", "dir", dir, "err", err)
		}
	}()

	inPath := filepath.Join(dir, "IN."+fromType)
	outPath := filepath.Join(dir, "IN."+toType)

	obj, err := d.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(req.FromBucket),
		Key:    aws.String(req.FromKey),
	})
	if err != nil {
		return fmt.Errorf("get %s/%s: %w", req.FromBucket, req.FromKey, err)
	}
	content, err := io.ReadAll(obj.Body)
	obj.Body.Close()
	if err != nil {
		return fmt.Errorf("read source body: %w", err)
	}
	if err := os.WriteFile(inPath, content, 0o600); err != nil {
		return fmt.Errorf("write input file: %w", err)
	}

	// Isolated user profile so concurrent conversions don't contend on the
	// shared soffice lock.
	profileDir := filepath.Join(dir, "louser")
	cctx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, d.jcfg.SofficePath,
		"--headless", "--norestore",
		"-env:UserInstallation=file://"+profileDir,
		"--convert-to", fmt.Sprintf("%s:%s", toType, filter),
		"--outdir", dir,
		inPath,
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("conversion timed out after %s", convertTimeout)
		}
		return fmt.Errorf("soffice convert failed: %w: %s", err, output.String())
	}

	result, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Errorf("read converted file %q: %w (soffice output: %s)", outPath, err, output.String())
	}

	if _, err := d.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(req.ToBucket),
		Key:    aws.String(req.ToKey),
		Body:   bytes.NewReader(result),
	}); err != nil {
		return fmt.Errorf("upload %s/%s: %w", req.ToBucket, req.ToKey, err)
	}
	return nil
}

// fileTypeFromKey mirrors `key.split('.').next_back() |> FileType::from_str`,
// restricted to the types the LOK filter table supports.
func fileTypeFromKey(key string) (string, bool) {
	ext := key
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		ext = key[i+1:]
	}
	switch strings.ToLower(ext) {
	case "docx", "pdf", "xlsx", "pptx", "html":
		return strings.ToLower(ext), true
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

// notifyConvertResult replaces the WEB_SOCKET_RESPONSE_LAMBDA invoke: the
// DocxUploadJobResult goes out on realtime.user.<owner> for the gateway to
// fan out. The owner is the first segment of the staging key; non-standard
// keys (e.g. the /internal/convert API) have no owner to notify.
func (d *deps) notifyConvertResult(ctx context.Context, req convertQueueMessage, success bool) error {
	parts, err := parseDocumentKey(req.FromKey)
	if err != nil {
		slog.Info("convert: from_key is not a docx staging key, skipping notify",
			"from_key", req.FromKey)
		return nil
	}
	if !strings.HasPrefix(parts.Owner, "macro|") {
		slog.Info("convert: non-user owner, skipping realtime notify",
			"owner", parts.Owner)
		return nil
	}

	var result docxUploadJobResult
	result.JobID = req.JobID
	result.JobType = "docx_upload" // Rust hardcodes this pending a job-type field
	if success {
		result.Status = "Completed"
		result.Data.Error = false
		// DocxUploadJobSuccessDataInner — the bogus bom part is a legacy
		// signal the frontend compares against.
		result.Data.Data = map[string]any{
			"converted": true,
			"bomParts": []map[string]any{{
				"sha":           "bogus",
				"path":          "bogus",
				"id":            "bogus",
				"documentBomId": 1,
			}},
		}
	} else {
		result.Status = "Failed"
		result.Data.Error = true
		msg := "error converting document"
		result.Data.Message = &msg
	}
	return d.publishRealtime(parts.Owner, "docx_upload.result", result)
}
