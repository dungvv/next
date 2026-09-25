package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/disintegration/imaging"

	"github.com/macro-inc/macro/pkg/events"
)

// asyncResizeRequest mirrors image_optimizer's AsyncResizeRequest payload —
// the self-invocation body the Lambda sent itself, now a jobs-stream message.
type asyncResizeRequest struct {
	OriginalKey      string `json:"original_key"`
	TransformedS3Key string `json:"transformed_s3_key"`
	Size             uint   `json:"size"`
}

// oneYearSeconds is DEFAULT_CACHE_TTL from the Rust service.
const oneYearSeconds = "31536000"

// maxOptimizeSize mirrors MAX_SIZE in the Rust transform.
const maxOptimizeSize = 4096

var imagingFormats = map[string]struct {
	format imaging.Format
	mime   string
}{
	"jpeg": {imaging.JPEG, "image/jpeg"},
	"png":  {imaging.PNG, "image/png"},
	"gif":  {imaging.GIF, "image/gif"},
	"tiff": {imaging.TIFF, "image/tiff"},
	"bmp":  {imaging.BMP, "image/bmp"},
}

// handleImageOptimize ports the AsyncResize arm of services/image_optimizer:
// fetch the original from the image bucket, resize it, and store the result
// at the variant key with a year-long cache header. If the transform fails
// the original is stored at the variant key instead so subsequent requests
// are served from MinIO without re-running the job.
func (d *deps) handleImageOptimize(ctx context.Context, env events.Envelope) error {
	var req asyncResizeRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return fmt.Errorf("decode image_optimize job: %w", err)
	}
	if req.OriginalKey == "" || req.TransformedS3Key == "" {
		return errors.New("image_optimize job missing keys")
	}
	if req.Size == 0 || req.Size > maxOptimizeSize {
		return fmt.Errorf("image_optimize size %d out of range", req.Size)
	}

	bucket := d.jcfg.ImageBucket
	out, err := d.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(req.OriginalKey),
	})
	if err != nil {
		return fmt.Errorf("fetch original %s/%s: %w", bucket, req.OriginalKey, err)
	}
	original, err := readAllAndClose(out.Body)
	if err != nil {
		return fmt.Errorf("read original body: %w", err)
	}
	originalMIME := aws.ToString(out.ContentType)
	if originalMIME == "" {
		originalMIME = "application/octet-stream"
	}

	body, mime := original, originalMIME
	transformed, format, terr := transformImage(original, req.Size)
	if terr != nil {
		slog.Warn("image_optimize: transform failed, caching original at variant key",
			"key", req.TransformedS3Key, "err", terr)
	} else {
		body, mime = transformed, format
	}

	if _, err := d.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(req.TransformedS3Key),
		Body:         bytes.NewReader(body),
		ContentType:  aws.String(mime),
		CacheControl: aws.String("public, max-age=" + oneYearSeconds),
	}); err != nil {
		return fmt.Errorf("store variant %s/%s: %w", bucket, req.TransformedS3Key, err)
	}
	return nil
}

// transformImage ports transform.rs: detect format from magic bytes, decode
// with EXIF auto-orientation, downscale the longest edge to size (never
// upscale), and re-encode in the source format.
func transformImage(data []byte, size uint) ([]byte, string, error) {
	_, formatName, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("detect image format: %w", err)
	}
	if formatName == "gif" {
		// Animated GIFs are returned as-is: frame-by-frame resize unsupported.
		return data, "image/gif", nil
	}
	out, ok := imagingFormats[formatName]
	if !ok {
		return nil, "", fmt.Errorf("format %q cannot be re-encoded", formatName)
	}

	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}

	bounds := img.Bounds()
	if bounds.Dx() > int(size) || bounds.Dy() > int(size) {
		img = imaging.Fit(img, int(size), int(size), imaging.Lanczos)
	}

	var buf bytes.Buffer
	opts := []imaging.EncodeOption{}
	if out.format == imaging.JPEG {
		opts = append(opts, imaging.JPEGQuality(75))
	}
	if err := imaging.Encode(&buf, img, out.format, opts...); err != nil {
		return nil, "", fmt.Errorf("encode %s: %w", formatName, err)
	}
	return buf.Bytes(), out.mime, nil
}
