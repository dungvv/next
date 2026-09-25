package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/macro-inc/macro/pkg/events"
)

// Redis keys, mirroring services/sha_cleanup_worker and
// crates/macro_sha_count_client.
const (
	shaCountKeyPrefix = "sha:"
	shaDeleteBucket   = "bucket:sha-delete"
)

// handleSHACleanup ports services/sha_cleanup_worker. Triggered by the
// scheduler; the envelope payload is ignored.
//
// Every sha in the "bucket:sha-delete" set is a candidate for deletion: if no
// BomPart rows reference it, the blob is removed from the document storage
// bucket; otherwise the cached count is refreshed. Successfully processed
// shas are removed from the set.
func (d *deps) handleSHACleanup(ctx context.Context, _ events.Envelope) error {
	shas, err := d.redis.SMembers(ctx, shaDeleteBucket).Result()
	if err != nil {
		return fmt.Errorf("read sha delete bucket: %w", err)
	}
	if len(shas) == 0 {
		slog.Info("sha_cleanup: delete bucket empty")
		return nil
	}
	slog.Info("sha_cleanup: potentially deleting shas", "count", len(shas))

	processed := make([]string, 0, len(shas))
	for _, sha := range shas {
		var count int64
		err := d.pools.MacroDB.QueryRow(ctx, `
			SELECT COUNT(*) FROM "BomPart" WHERE sha = $1
		`, sha).Scan(&count)
		if err != nil {
			slog.Error("sha_cleanup: sha count failed", "sha", sha, "err", err)
			continue
		}

		if count != 0 {
			// set_sha_count: SETNX sha:<sha> 0 then INCRBY count.
			if err := d.redis.SetNX(ctx, shaCountKeyPrefix+sha, 0, 0).Err(); err != nil {
				slog.Error("sha_cleanup: setnx failed", "sha", sha, "err", err)
				continue
			}
			if err := d.redis.IncrBy(ctx, shaCountKeyPrefix+sha, count).Err(); err != nil {
				slog.Error("sha_cleanup: incrby failed", "sha", sha, "err", err)
				continue
			}
			processed = append(processed, sha)
			continue
		}

		if _, err := d.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(d.jcfg.DocumentStorageBucket),
			Key:    aws.String(sha),
		}); err != nil {
			slog.Error("sha_cleanup: s3 delete failed", "sha", sha, "err", err)
			continue
		}
		processed = append(processed, sha)
	}

	if len(processed) > 0 {
		if err := d.redis.SRem(ctx, shaDeleteBucket, processed).Err(); err != nil {
			return fmt.Errorf("remove processed shas from set: %w", err)
		}
	}
	return nil
}
