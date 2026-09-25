// Package jobs implements the "macro worker" subcommand: JetStream pull
// consumers replacing the ex-Lambda handlers and ex-Kafka consumers
// (docs/GO_SELFHOST_PLAN.md §E).
//
// Every handler binds one durable pull consumer on the "jobs" work-queue
// stream, filtered on jobs.<name>. Periodic jobs that were EventBridge cron
// triggers are published to their subject by `macro scheduler`; object-driven
// jobs (docx_unzip, call_recording_preview) are published by the upload
// pipeline / MinIO bucket-notification relay.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/errgroup"

	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
	"github.com/macro-inc/macro/pkg/store"
)

// consumerSpec binds a durable name, filter subject, and handler.
type consumerSpec struct {
	durable string
	subject string
	handler Handler
}

// Run starts every jobs-stream consumer concurrently and blocks until ctx is
// cancelled or a consumer fails.
func Run(ctx context.Context, cfg config.Config) error {
	nc, js, err := natsx.Connect(ctx, cfg.NatsURL)
	if err != nil {
		return err
	}

	pools, err := store.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open db pools: %w", err)
	}
	defer pools.Close()

	rdb, err := newRedisClient(cfg)
	if err != nil {
		return fmt.Errorf("redis client: %w", err)
	}
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}

	s3c := newS3Client(cfg)
	d := &deps{
		cfg:        cfg,
		jcfg:       loadJobConfig(),
		nc:         nc,
		js:         js,
		pools:      pools,
		s3:         s3c,
		presign:    s3.NewPresignClient(s3c),
		redis:      rdb,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	// Event streams (ex-Kafka topics) that the pollers publish to. Ensuring
	// them here is idempotent and keeps publishes from failing on a fresh
	// deployment.
	for _, stream := range []string{events.StreamDocuments, events.StreamChats, streamProjects} {
		if _, err := natsx.EnsureEventStream(ctx, js, stream, []string{stream + ".>"}); err != nil {
			return fmt.Errorf("ensure event stream %q: %w", stream, err)
		}
	}

	consumers := []consumerSpec{
		{"deleted_item_poll", subjDeletedItemPoll, d.handleDeletedItemPoll},
		{"user_link_cleanup", subjUserLinkCleanup, d.handleUserLinkCleanup},
		{"sha_cleanup", subjSHACleanup, d.handleSHACleanup},
		{"docx_unzip", subjDocxUnzip, d.handleDocxUnzip},
		{"image_optimize", subjImageOptimize, d.handleImageOptimize},
		{"call_recording_preview", subjCallRecordingPreview, d.handleCallRecordingPreview},
		{"email_sfs_delete", subjEmailSFSScanOrphans, d.handleEmailSFSScan},
		// Downstream work-queue subjects other services publish to.
		{"sfs_delete", subjSFSDelete, d.handleSFSDelete},
		{"delete_chat", subjDeleteChat, d.handleDeleteChat},
		{"delete_document", subjDeleteDocument, d.handleDeleteDocument},
		{"convert", subjConvert, d.handleConvert},
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, c := range consumers {
		g.Go(func() error {
			err := Consume(ctx, js, events.StreamJobs, c.durable, c.subject, c.handler)
			if err != nil {
				return fmt.Errorf("consumer %q: %w", c.durable, err)
			}
			return nil
		})
	}

	slog.Info("worker started", "consumers", len(consumers))
	return g.Wait()
}
