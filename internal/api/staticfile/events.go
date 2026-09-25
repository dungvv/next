package staticfile

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"

	"github.com/nats-io/nats.go"
)

// S3Event mirrors the object-created notification shape shared by AWS S3
// event notifications and MinIO's NATS bucket-notification target.
type s3EventEnvelope struct {
	EventName string `json:"EventName"`
	Records   []struct {
		EventName string `json:"eventName"`
		S3        struct {
			Object struct {
				Key string `json:"key"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

// SubscribeUploadEvents subscribes to MinIO bucket notifications on subject
// (e.g. "s3.events") and marks matching static-file metadata rows uploaded.
// It replaces the Rust SQS poller (api/event/poll.rs + s3_create.rs).
//
// Configure MinIO with a NATS notify target pointing at this subject for the
// static storage bucket.
func SubscribeUploadEvents(ctx context.Context, nc *nats.Conn, subject string, meta MetadataStore) {
	_, err := nc.QueueSubscribe(subject, "static-file-service", func(msg *nats.Msg) {
		var ev s3EventEnvelope
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			slog.Warn("staticfile: could not parse s3 event", "err", err)
			return
		}
		for _, rec := range ev.Records {
			if !strings.HasPrefix(rec.EventName, "s3:ObjectCreated") {
				continue
			}
			handleS3Create(ctx, rec.S3.Object.Key, meta)
		}
	})
	if err != nil {
		slog.Error("staticfile: subscribe upload events", "err", err)
	}
	go func() {
		<-ctx.Done()
		_ = nc.Drain()
	}()
}

// handleS3Create ports api/event/s3_create.rs::handle_s3_create.
func handleS3Create(ctx context.Context, rawKey string, meta MetadataStore) {
	// S3 event keys arrive form-encoded (MinIO and classic S3 notifications).
	key, err := url.QueryUnescape(rawKey)
	if err != nil {
		key = rawKey
	}
	// Skip CloudFront image optimizer keys (e.g. "file/{uuid}/format=webp,width=300").
	if strings.Count(key, "/") > 1 {
		return
	}
	parsed, err := ParseStaticFileKey(key)
	if err != nil {
		slog.Error("staticfile: unexpected s3 key format", "key", key, "err", err)
		return
	}
	if err := meta.MarkUploaded(ctx, parsed.FileID); err != nil {
		slog.Error("staticfile: could not mark file uploaded",
			"file_id", parsed.FileID, "err", err)
	}
}
