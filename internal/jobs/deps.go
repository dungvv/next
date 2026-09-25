package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/store"
)

// Event stream for project lifecycle events. Missing from the pkg/events
// catalog; defined locally until the catalog is extended.
const streamProjects = "macro.projects"

// Job subjects on the jobs work-queue stream. Consumers below bind one
// durable per subject; sibling producers (scheduler, upload pipeline,
// MinIO notifications) publish to these subjects.
const (
	subjDeletedItemPoll      = "jobs.deleted_item_poll"
	subjUserLinkCleanup      = "jobs.user_link_cleanup"
	subjSHACleanup           = "jobs.sha_cleanup"
	subjDocxUnzip            = "jobs.docx_unzip"
	subjImageOptimize        = "jobs.image_optimize"
	subjCallRecordingPreview = "jobs.call_recording_preview"
	subjEmailSFSScanOrphans  = "jobs.email_sfs_delete"
	subjSFSDelete            = "jobs.sfs_delete"
	subjDeleteChat           = "jobs.delete_chat"
	subjDeleteDocument       = "jobs.delete_document"
	subjConvert              = "jobs.convert"
)

// deps holds the shared clients available to every job handler.
type deps struct {
	cfg     config.Config
	jcfg    jobConfig
	nc      *nats.Conn
	js      jetstream.JetStream
	pools   *store.Pools
	s3      *s3.Client
	presign *s3.PresignClient
	redis   *redis.Client
	// httpClient is for internal service HTTP calls (e.g. sfs_delete hitting
	// the static file service's /internal routes).
	httpClient *http.Client
}

// jobConfig carries job-specific env vars that pkg/config does not model yet.
// The Rust services read these via macro_env_var; until pkg/config grows the
// fields, they are parsed here.
type jobConfig struct {
	// DOCUMENT_STORAGE_BUCKET — bucket holding document bytes and bom parts.
	DocumentStorageBucket string
	// DOCX_STAGING_BUCKET — fallback bucket for docx_unzip jobs whose payload
	// omits the bucket (Rust: S3 event supplied it).
	DocxStagingBucket string
	// CALL_RECORDING_BUCKET_NAME — fallback bucket for call recording jobs
	// whose payload omits the bucket.
	CallRecordingBucket string
	// IMAGE_BUCKET — bucket the image optimizer reads/writes (Rust env: BUCKET).
	ImageBucket string
	// FFMPEG_PATH / FFPROBE_PATH — binary paths; the image bundles ffmpeg.
	FfmpegPath  string
	FfprobePath string
	// SOFFICE_PATH — LibreOffice binary for jobs.convert. The Rust service
	// linked LibreOfficeKit via LOK_PATH; the Go port shells out to soffice,
	// so the worker image must ship LibreOffice.
	SofficePath string
	// STATIC_FILE_SERVICE_URL — base URL of the static file service used by
	// jobs.sfs_delete (Rust: StaticFileServiceUrl / SFS_URL). Defaults to the
	// monolith API on the same host.
	StaticFileServiceURL string
	// INTERNAL_API_KEY — x-internal-auth-key header for internal HTTP calls.
	InternalAPIKey string
}

func loadJobConfig() jobConfig {
	return jobConfig{
		DocumentStorageBucket: envOr("DOCUMENT_STORAGE_BUCKET", "documents"),
		DocxStagingBucket:     envOr("DOCX_STAGING_BUCKET", "docx-staging"),
		CallRecordingBucket:   envOr("CALL_RECORDING_BUCKET_NAME", "call-recordings"),
		ImageBucket:           envOr("IMAGE_BUCKET", "images"),
		FfmpegPath:            envOr("FFMPEG_PATH", "ffmpeg"),
		FfprobePath:           envOr("FFPROBE_PATH", "ffprobe"),
		SofficePath:           envOr("SOFFICE_PATH", "soffice"),
		StaticFileServiceURL:  envOr("STATIC_FILE_SERVICE_URL", "http://localhost:8080"),
		InternalAPIKey:        envOr("INTERNAL_API_KEY", ""),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// newS3Client builds an S3 client pointed at MinIO (path-style, static creds).
func newS3Client(cfg config.Config) *s3.Client {
	return s3.New(s3.Options{
		Region: cfg.S3Region,
		Credentials: aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(cfg.S3AccessKeyID, cfg.S3SecretAccessKey, ""),
		),
		BaseEndpoint: aws.String(cfg.S3Endpoint),
		UsePathStyle: true,
	})
}

// newRedisClient builds a Valkey client. REDIS_URI (e.g. redis://host:6379)
// takes precedence over cfg.ValkeyAddr to match the Rust services' env name.
func newRedisClient(cfg config.Config) (*redis.Client, error) {
	if uri := os.Getenv("REDIS_URI"); uri != "" {
		opt, err := redis.ParseURL(uri)
		if err != nil {
			return nil, fmt.Errorf("parse REDIS_URI: %w", err)
		}
		return redis.NewClient(opt), nil
	}
	return redis.NewClient(&redis.Options{Addr: cfg.ValkeyAddr}), nil
}

// publishJob marshals an envelope and publishes it to a JetStream subject.
func (d *deps) publishJob(ctx context.Context, subject, typ, aggregate string, data any) error {
	return d.publishJobID(ctx, subject, typ, aggregate, "", data)
}

// publishJobID is publishJob with a JetStream Nats-Msg-Id dedup key: the
// stream drops repeats of msgID inside its duplicate window, which keeps
// at-least-once redeliveries of the same parent job from enqueueing
// duplicate downstream work. Pass "" to disable dedup.
func (d *deps) publishJobID(ctx context.Context, subject, typ, aggregate, msgID string, data any) error {
	env, err := events.New(typ, "jobs", aggregate, 1, data)
	if err != nil {
		return fmt.Errorf("build envelope: %w", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	m := nats.NewMsg(subject)
	if msgID != "" {
		m.Header.Set("Nats-Msg-Id", msgID)
	}
	m.Data = raw
	if _, err := d.js.PublishMsg(ctx, m); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// publishEvent publishes an envelope on a subject-sharded event stream
// (ex-Kafka topic), e.g. publishEvent("macro.documents", docID, "document.purged", meta).
func (d *deps) publishEvent(ctx context.Context, stream, aggregateID, typ string, data any) error {
	return d.publishJob(ctx, events.Shard(stream, aggregateID), typ, aggregateID, data)
}

// publishRealtime publishes a fire-and-forget core-NATS message on
// realtime.user.<user_id> for the gateway to fan out to websockets.
func (d *deps) publishRealtime(userID string, typ string, data any) error {
	env, err := events.New(typ, "jobs", userID, 1, data)
	if err != nil {
		return fmt.Errorf("build envelope: %w", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	subject := fmt.Sprintf("realtime.user.%s", userID)
	if err := d.nc.Publish(subject, raw); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}
