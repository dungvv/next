package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/macro-inc/macro/pkg/events"
)

const (
	callsPrefix          = "calls/"
	mp4Suffix            = ".mp4"
	previewFilename      = "PREVIEW.jpg"
	jpegContentType      = "image/jpeg"
	presignDuration      = 15 * time.Minute // Rust: DEFAULT_PRESIGNED_URL_SECONDS = 900
	ffmpegCommandTimeout = 60 * time.Second
)

// callRecordingJob is the envelope payload for subjCallRecordingPreview,
// replacing the S3 event record. Key may arrive form-encoded (as S3
// notifications encode it) and is decoded before parsing.
type callRecordingJob struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

type previewKeys struct {
	sourceKey    string
	recordingKey string // key without the calls/ prefix; stored on call rows
	previewKey   string
}

// handleCallRecordingPreview ports services/call_recording_preview_handler:
// for an mp4 under calls/, probe its duration with ffprobe, extract the
// midpoint frame with ffmpeg (falling back to t=0), upload PREVIEW.jpg next
// to the recording, and persist the preview key on calls/call_records rows.
func (d *deps) handleCallRecordingPreview(ctx context.Context, env events.Envelope) error {
	var job callRecordingJob
	if err := json.Unmarshal(env.Data, &job); err != nil {
		return fmt.Errorf("decode call_recording_preview job: %w", err)
	}
	if job.Key == "" {
		return errors.New("call_recording_preview job missing key")
	}
	bucket := job.Bucket
	if bucket == "" {
		bucket = d.jcfg.CallRecordingBucket
	}
	if bucket == "" {
		return errors.New("call_recording_preview job missing bucket")
	}

	keys, skip, err := previewKeysFromEncodedKey(job.Key)
	if err != nil {
		return err
	}
	if skip != "" {
		slog.Info("call_recording_preview: skipping object", "key", job.Key, "reason", skip)
		return nil
	}
	log := slog.With("bucket", bucket, "source_key", keys.sourceKey, "preview_key", keys.previewKey)
	log.Info("generating call recording preview")

	sourceURL, err := d.presignGet(ctx, bucket, keys.sourceKey)
	if err != nil {
		return err
	}

	outputPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("call-recording-preview-%d-%d.jpg", os.Getpid(), time.Now().UnixNano()))
	defer os.Remove(outputPath)

	if err := d.createPreviewJPEG(ctx, sourceURL, outputPath); err != nil {
		return fmt.Errorf("create preview for %s: %w", keys.sourceKey, err)
	}

	body, err := os.ReadFile(outputPath)
	if err != nil {
		return fmt.Errorf("read preview %s: %w", outputPath, err)
	}
	if _, err := d.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(keys.previewKey),
		ContentType: aws.String(jpegContentType),
		Body:        bytes.NewReader(body),
	}); err != nil {
		return fmt.Errorf("upload s3://%s/%s: %w", bucket, keys.previewKey, err)
	}

	updated, err := d.updatePreviewKey(ctx, keys.recordingKey, keys.previewKey)
	if err != nil {
		return fmt.Errorf("persist preview key for %s: %w", keys.recordingKey, err)
	}
	if updated == 0 {
		return fmt.Errorf("no call rows matched recording_key %s", keys.recordingKey)
	}
	return nil
}

// previewKeysFromEncodedKey ports key.rs: decode the (possibly form-encoded)
// key and derive source/recording/preview keys, or a skip reason.
func previewKeysFromEncodedKey(encodedKey string) (previewKeys, string, error) {
	decoded, err := decodeS3ObjectKey(encodedKey)
	if err != nil {
		return previewKeys{}, "", err
	}
	recordingKey, ok := strings.CutPrefix(decoded, callsPrefix)
	if !ok {
		return previewKeys{}, "object key is outside calls/", nil
	}
	idx := strings.LastIndex(recordingKey, "/")
	if idx < 0 {
		return previewKeys{}, "object key has no call recording parent", nil
	}
	parent, fileName := recordingKey[:idx], recordingKey[idx+1:]
	if parent == "" || fileName == "" {
		return previewKeys{}, "object key has no call recording parent", nil
	}
	if fileName == previewFilename {
		return previewKeys{}, "object key is the generated preview image", nil
	}
	stem, ok := strings.CutSuffix(fileName, mp4Suffix)
	if !ok {
		return previewKeys{}, "object key is not an mp4 recording", nil
	}
	return previewKeys{
		sourceKey:    decoded,
		recordingKey: recordingKey,
		previewKey:   fmt.Sprintf("%s%s/%s/%s", callsPrefix, parent, stem, previewFilename),
	}, "", nil
}

// decodeS3ObjectKey ports decode_s3_object_key: '+' becomes space, then the
// key is percent-decoded.
func decodeS3ObjectKey(encodedKey string) (string, error) {
	formEncoded := strings.ReplaceAll(encodedKey, "+", "%20")
	decoded, err := url.PathUnescape(formEncoded)
	if err != nil {
		return "", fmt.Errorf("decode S3 object key %q: %w", encodedKey, err)
	}
	return decoded, nil
}

func (d *deps) presignGet(ctx context.Context, bucket, key string) (string, error) {
	out, err := d.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(presignDuration))
	if err != nil {
		return "", fmt.Errorf("presign s3://%s/%s: %w", bucket, key, err)
	}
	return out.URL, nil
}

// createPreviewJPEG ports FfmpegTools::create_preview_jpeg.
func (d *deps) createPreviewJPEG(ctx context.Context, sourceURL, outputPath string) error {
	duration, err := d.probeDuration(ctx, sourceURL)
	if err != nil {
		return err
	}
	return d.extractFrameWithFallback(ctx, sourceURL, duration/2.0, outputPath)
}

// probeDuration ports ffprobe duration probing.
func (d *deps) probeDuration(ctx context.Context, sourceURL string) (float64, error) {
	out, err := runCommand(ctx, d.jcfg.FfprobePath,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		sourceURL,
	)
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		d, err := strconv.ParseFloat(line, 64)
		if err != nil {
			return 0, fmt.Errorf("parse ffprobe duration %q: %w", line, err)
		}
		if math.IsInf(d, 0) || math.IsNaN(d) || d < 0 {
			return 0, fmt.Errorf("ffprobe returned invalid duration %v", d)
		}
		return d, nil
	}
	return 0, errors.New("ffprobe did not return a duration")
}

// extractFrameWithFallback ports the midpoint-then-start retry.
func (d *deps) extractFrameWithFallback(ctx context.Context, sourceURL string, midpoint float64, outputPath string) error {
	_ = removeIfPresent(outputPath)

	midErr := d.extractFrame(ctx, sourceURL, midpoint, outputPath)
	hasData, err := fileHasData(outputPath)
	if err != nil {
		return err
	}
	if hasData {
		return nil
	}
	if midErr != nil {
		slog.Warn("midpoint ffmpeg extraction failed; retrying at start", "err", midErr)
	} else {
		slog.Warn("midpoint ffmpeg extraction produced no frame; retrying at start")
	}

	_ = removeIfPresent(outputPath)
	if err := d.extractFrame(ctx, sourceURL, 0, outputPath); err != nil {
		return fmt.Errorf("extract preview frame at start: %w", err)
	}
	hasData, err = fileHasData(outputPath)
	if err != nil {
		return err
	}
	if !hasData {
		return errors.New("ffmpeg completed but did not create a preview frame")
	}
	return nil
}

func (d *deps) extractFrame(ctx context.Context, sourceURL string, seekSeconds float64, outputPath string) error {
	_, err := runCommand(ctx, d.jcfg.FfmpegPath,
		"-y",
		"-ss", fmt.Sprintf("%.3f", seekSeconds),
		"-i", sourceURL,
		"-frames:v", "1",
		"-q:v", "2",
		outputPath,
	)
	return err
}

// runCommand runs a binary with a 60s timeout, capturing combined output.
func runCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ffmpegCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s failed: %w: %s", filepath.Base(path), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func fileHasData(path string) (bool, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return st.Size() > 0, nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// updatePreviewKey ports db.rs::update_preview_key: sets preview_url on both
// the active calls table and archived call_records in one transaction.
func (d *deps) updatePreviewKey(ctx context.Context, recordingKey, previewKey string) (int64, error) {
	tx, err := d.pools.MacroDB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	active, err := tx.Exec(ctx, `UPDATE calls SET preview_url = $2 WHERE recording_key = $1`, recordingKey, previewKey)
	if err != nil {
		return 0, err
	}
	archived, err := tx.Exec(ctx, `UPDATE call_records SET preview_url = $2 WHERE recording_key = $1`, recordingKey, previewKey)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return active.RowsAffected() + archived.RowsAffected(), nil
}

func readAllAndClose(r io.ReadCloser) ([]byte, error) {
	defer r.Close()
	return io.ReadAll(r)
}
