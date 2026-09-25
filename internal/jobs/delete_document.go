package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/macro-inc/macro/pkg/events"
)

// deleteDocumentJob is the envelope payload for subjDeleteDocument.
// document_id maps to the Rust SQS message attribute of the same name;
// user_id keeps its historical wire name but carries the owner principal
// ("macro|<email>", "bot|<uuid>", or a team UUID) when the producer already
// knows it — the with-owner enqueue path skips the DB lookup/delete because
// the document row is presumed already gone.
type deleteDocumentJob struct {
	DocumentID string `json:"document_id"`
	Owner      string `json:"user_id"`
}

// handleDeleteDocument ports document_storage_service's
// delete_document_worker: hard-delete the document row (decrementing docx
// bom-part sha counts), clean up entity mentions, then remove the document's
// objects from the document storage bucket.
//
// Stubbed relative to Rust (no self-host equivalent exists yet; each is
// logged and the message still acks):
//   - sync_service_client.delete(document_id) — internal/syncsvc exposes no
//     per-document delete endpoint
//   - editing_worker_client.delete_traces — ai-editing-worker is not ported
//   - properties_service.delete_entity_properties — properties service is
//     not ported
func (d *deps) handleDeleteDocument(ctx context.Context, env events.Envelope) error {
	var job deleteDocumentJob
	if err := json.Unmarshal(env.Data, &job); err != nil {
		return fmt.Errorf("decode delete_document job: %w", err)
	}
	if job.DocumentID == "" {
		return errors.New("delete_document job missing document_id")
	}
	log := slog.With("document_id", job.DocumentID)

	owner := job.Owner
	if owner == "" {
		// Owner unknown: the document row must still be deleted here.
		info, err := d.deletedDocumentInfo(ctx, job.DocumentID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The row is already gone — a previous delivery finished the
				// DB delete before failing, or the document never existed.
				// Without the owner the S3 prefix can't be located anyway,
				// so ack rather than retry forever. (Rust retried until the
				// DLQ; the end state is identical.)
				log.Warn("delete_document: document row already gone, skipping")
				return nil
			}
			return fmt.Errorf("get deleted document info: %w", err)
		}
		owner = info.owner
		log = log.With("owner", owner, "file_type", info.fileType)

		if info.fileType != nil && *info.fileType == "docx" {
			shas, err := d.documentBomSHAs(ctx, job.DocumentID)
			if err != nil {
				return fmt.Errorf("get bom parts: %w", err)
			}
			if err := d.decrementSHACounts(ctx, countOccurrences(shas)); err != nil {
				return fmt.Errorf("decrement sha counts: %w", err)
			}
		}

		if err := deleteDocumentTx(ctx, d.pools.MacroDB, job.DocumentID); err != nil {
			return fmt.Errorf("delete document row: %w", err)
		}
		log.Info("delete_document: document row deleted")
	}

	if !validOwnerPrincipal(owner) {
		return fmt.Errorf("invalid owner principal %q for document %s", owner, job.DocumentID)
	}

	// comms_db_client::entity_mentions::delete_entity_mentions_by_source —
	// best-effort, mirroring the Rust `let _ = ... inspect_err`.
	if _, err := d.pools.CommsDB.Exec(ctx, `
		DELETE FROM comms_entity_mentions WHERE source_entity_id = ANY($1::varchar[])
	`, []string{job.DocumentID}); err != nil {
		log.Warn("delete_document: could not delete entity mentions", "err", err)
	}

	// s3_client.delete_document: remove every object under {owner}/{doc_id}.
	if err := d.deleteDocumentObjects(ctx, owner, job.DocumentID); err != nil {
		return fmt.Errorf("delete files from s3: %w", err)
	}

	// Rust also deletes the document from sync service, AI edit traces, and
	// document properties — none of those services exist in the self-host
	// port yet; the calls were already best-effort there.
	log.Info("delete_document: sync/editing/properties cleanup not ported, skipping")

	return nil
}

type deletedDocumentInfo struct {
	owner    string
	fileType *string
}

// deletedDocumentInfo is the subset of
// macro_db_client::document::get_deleted_document_info the worker needs.
func (d *deps) deletedDocumentInfo(ctx context.Context, documentID string) (deletedDocumentInfo, error) {
	var info deletedDocumentInfo
	err := d.pools.MacroDB.QueryRow(ctx, `
		SELECT d.owner, d."fileType" FROM "Document" d WHERE d.id = $1
	`, documentID).Scan(&info.owner, &info.fileType)
	return info, err
}

// documentBomSHAs ports macro_db_client::document::get_bom_parts (sha column
// only — the worker just needs them for the sha refcount decrement).
func (d *deps) documentBomSHAs(ctx context.Context, documentID string) ([]string, error) {
	rows, err := d.pools.MacroDB.Query(ctx, `
		SELECT bp.sha
		FROM "BomPart" bp
		JOIN "DocumentBom" db ON bp."documentBomId" = db.id
		WHERE db."documentId" = $1
	`, documentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var sha string
		return sha, row.Scan(&sha)
	})
}

// countOccurrences ports the count_occurrences helper in the Rust worker:
// each bom part referencing a sha decrements it once.
func countOccurrences(shas []string) map[string]int64 {
	counts := make(map[string]int64, len(shas))
	for _, s := range shas {
		counts[s]++
	}
	return counts
}

// decrementSHACounts ports macro_sha_count_client::decrement_counts: when a
// sha's cached count hits 0 the count key is dropped and the sha joins the
// sha-delete set for sha_cleanup to reap.
func (d *deps) decrementSHACounts(ctx context.Context, shaCounts map[string]int64) error {
	for sha, dec := range shaCounts {
		key := shaCountKeyPrefix + sha
		count, err := d.redis.Get(ctx, key).Int64()
		if errors.Is(err, redis.Nil) {
			count = 0
		} else if err != nil {
			return fmt.Errorf("get sha count %s: %w", sha, err)
		}

		if count == 0 || count-dec <= 0 {
			if err := d.redis.Del(ctx, key).Err(); err != nil {
				return fmt.Errorf("delete sha count %s: %w", sha, err)
			}
			if err := d.redis.SAdd(ctx, shaDeleteBucket, sha).Err(); err != nil {
				return fmt.Errorf("add sha %s to delete bucket: %w", sha, err)
			}
			continue
		}

		if err := d.redis.DecrBy(ctx, key, dec).Err(); err != nil {
			return fmt.Errorf("decr sha %s: %w", sha, err)
		}
	}
	return nil
}

// deleteDocumentTx ports macro_db_client::document::delete_document: pins,
// history, share permission (via DocumentPermission), the document row,
// entity_access rows, then the entity registry row — one transaction.
func deleteDocumentTx(ctx context.Context, db *pgxpool.Pool, documentID string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		DELETE FROM "Pin" WHERE "pinnedItemId" = $1 AND "pinnedItemType" = 'document'
	`, documentID); err != nil {
		return fmt.Errorf("delete pins: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM "UserHistory" WHERE "itemId" = $1 AND "itemType" = 'document'
	`, documentID); err != nil {
		return fmt.Errorf("delete user history: %w", err)
	}

	var sharePermissionID *string
	err = tx.QueryRow(ctx, `
		SELECT "sharePermissionId" FROM "DocumentPermission" WHERE "documentId" = $1
	`, documentID).Scan(&sharePermissionID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get document permission: %w", err)
	}
	if sharePermissionID != nil {
		if _, err := tx.Exec(ctx, `
			DELETE FROM "SharePermission" WHERE id = $1
		`, *sharePermissionID); err != nil {
			return fmt.Errorf("delete share permission: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM "Document" WHERE id = $1
	`, documentID); err != nil {
		return fmt.Errorf("delete document row: %w", err)
	}

	// delete_user_entity_access_by_item — Rust unwraps string_to_uuid here,
	// so a malformed id fails the whole handler.
	docUUID, err := uuid.Parse(documentID)
	if err != nil {
		return fmt.Errorf("parse document uuid %q: %w", documentID, err)
	}
	u := pgtype.UUID{Bytes: docUUID, Valid: true}
	if _, err := tx.Exec(ctx, `
		DELETE FROM entity_access WHERE entity_id = $1 AND entity_type = 'document'
	`, u); err != nil {
		return fmt.Errorf("delete entity access: %w", err)
	}

	// entity_registry_db_utils::delete_entity (Rust wraps it in `if let Ok`,
	// but the uuid already parsed above so it always runs here).
	if _, err := tx.Exec(ctx, `
		DELETE FROM entity WHERE id = $1
	`, u); err != nil {
		return fmt.Errorf("delete entity row: %w", err)
	}

	return tx.Commit(ctx)
}

// deleteDocumentObjects ports s3_client.delete_document: list every object
// under {owner_principal}/{document_id} in the document storage bucket and
// delete them. The owner segment is the principal string verbatim
// (s3_key::build_cloud_storage_bucket_document_prefix).
func (d *deps) deleteDocumentObjects(ctx context.Context, owner, documentID string) error {
	bucket := d.jcfg.DocumentStorageBucket
	prefix := owner + "/" + documentID

	var token *string
	for {
		out, err := d.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("list %s/%s*: %w", bucket, prefix, err)
		}
		if len(out.Contents) > 0 {
			ids := make([]types.ObjectIdentifier, 0, len(out.Contents))
			for _, obj := range out.Contents {
				ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
			}
			if _, err := d.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &types.Delete{Objects: ids},
			}); err != nil {
				return fmt.Errorf("delete objects under %s/%s: %w", bucket, prefix, err)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		token = out.NextContinuationToken
	}
}
