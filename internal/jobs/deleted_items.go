package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/macro-inc/macro/pkg/events"
)

// deletedItemPollRetention mirrors the Rust poller: rows soft-deleted more
// than 30 days ago are permanently deleted.
const deletedItemPollRetention = 30 * 24 * time.Hour

// handleDeletedItemPoll ports services/deleted_item_poller. Triggered by the
// scheduler (ex-EventBridge cron); the envelope payload is ignored.
//
// For each entity kind it publishes a lifecycle event on the subject-sharded
// event stream (ex-Kafka topic), then either enqueues a delete job on the
// jobs stream (chats/documents — consumed by the delete_chat/document
// pipeline) or hard-deletes directly (projects, which cascade-queue their
// children via the jobs the project delete fans out downstream).
func (d *deps) handleDeletedItemPoll(ctx context.Context, _ events.Envelope) error {
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return d.purgeExpiredProjects(ctx) })
	g.Go(func() error { return d.purgeExpiredChats(ctx) })
	g.Go(func() error { return d.purgeExpiredDocuments(ctx) })
	return g.Wait()
}

type projectToDelete struct {
	ProjectID string
	UserID    string
}

func (d *deps) purgeExpiredProjects(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-deletedItemPollRetention)

	rows, err := d.pools.MacroDB.Query(ctx, `
		SELECT p.id AS project_id, p."userId" AS user_id
		FROM "Project" p
		WHERE p."deletedAt" IS NOT NULL AND p."deletedAt" <= $1
	`, cutoff)
	if err != nil {
		return fmt.Errorf("query projects to delete: %w", err)
	}
	projects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (projectToDelete, error) {
		var p projectToDelete
		return p, row.Scan(&p.ProjectID, &p.UserID)
	})
	if err != nil {
		return fmt.Errorf("scan projects to delete: %w", err)
	}
	if len(projects) == 0 {
		slog.Info("deleted_item_poll: no projects to delete")
		return nil
	}
	slog.Debug("deleted_item_poll: projects to delete", "count", len(projects))

	// project.permanently_deleted — mirrors ProjectMacroEvent::permanently_deleted.
	// The owner is serialized as its principal string (user_id column); Rust
	// parses it via Owner::from_principal_str, so an invalid principal fails
	// the whole batch here too.
	for _, p := range projects {
		if !validOwnerPrincipal(p.UserID) {
			return fmt.Errorf("invalid owner for project %s: %q", p.ProjectID, p.UserID)
		}
		err := d.publishEvent(ctx, streamProjects, p.ProjectID, "project.permanently_deleted", map[string]any{
			"project_id":          p.ProjectID,
			"owner":               p.UserID,
			"actor_user_id":       nil,
			"parent_project_id":   nil,
			"purged_project_ids":  []string{p.ProjectID},
			"purged_document_ids": []string{},
			"purged_chat_ids":     []string{},
		})
		if err != nil {
			return fmt.Errorf("publish project purge event %s: %w", p.ProjectID, err)
		}
	}

	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ProjectID
	}
	if err := deleteProjectsBulk(ctx, d.pools.MacroDB, ids); err != nil {
		return fmt.Errorf("delete projects: %w", err)
	}
	return nil
}

// validOwnerPrincipal mirrors Owner::from_principal_str: "macro|" users,
// "bot|" bots, otherwise a hyphenated team UUID.
func validOwnerPrincipal(principal string) bool {
	if strings.HasPrefix(principal, "macro|") || strings.HasPrefix(principal, "bot|") {
		return true
	}
	_, err := uuid.Parse(principal)
	return err == nil
}

// toUUIDs parses id strings into pgtype.UUID values for uuid[] parameters.
// Matches Rust's string_to_uuid().unwrap(): a malformed id aborts the batch.
func toUUIDs(ids []string) ([]pgtype.UUID, error) {
	out := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("parse uuid %q: %w", id, err)
		}
		out[i] = pgtype.UUID{Bytes: u, Valid: true}
	}
	return out, nil
}

func (d *deps) purgeExpiredChats(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-deletedItemPollRetention)

	rows, err := d.pools.MacroDB.Query(ctx, `
		SELECT c.id
		FROM "Chat" c
		WHERE c."deletedAt" IS NOT NULL AND c."deletedAt" <= $1
	`, cutoff)
	if err != nil {
		return fmt.Errorf("query chats to delete: %w", err)
	}
	chatIDs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var id string
		return id, row.Scan(&id)
	})
	if err != nil {
		return fmt.Errorf("scan chats to delete: %w", err)
	}
	if len(chatIDs) == 0 {
		slog.Info("deleted_item_poll: no chats to delete")
		return nil
	}

	// chat.permanently_deleted — mirrors ChatMacroEvent::permanently_deleted.
	for _, id := range chatIDs {
		err := d.publishEvent(ctx, events.StreamChats, id, "chat.permanently_deleted", map[string]any{
			"chat_id":       id,
			"actor_user_id": nil,
			"project_id":    nil,
		})
		if err != nil {
			return fmt.Errorf("publish chat purge event %s: %w", id, err)
		}
	}

	// The SQS chat-delete queue becomes a jobs-stream subject; the delete_chat
	// consumer owns the actual teardown.
	for _, id := range chatIDs {
		if err := d.publishJob(ctx, subjDeleteChat, "chat.delete", id, map[string]any{
			"chat_id": id,
		}); err != nil {
			return fmt.Errorf("enqueue chat delete %s: %w", id, err)
		}
	}
	return nil
}

func (d *deps) purgeExpiredDocuments(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-deletedItemPollRetention)

	rows, err := d.pools.MacroDB.Query(ctx, `
		SELECT d.id
		FROM "Document" d
		WHERE d."deletedAt" IS NOT NULL AND d."deletedAt" <= $1
	`, cutoff)
	if err != nil {
		return fmt.Errorf("query documents to delete: %w", err)
	}
	docIDs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var id string
		return id, row.Scan(&id)
	})
	if err != nil {
		return fmt.Errorf("scan documents to delete: %w", err)
	}
	if len(docIDs) == 0 {
		slog.Info("deleted_item_poll: no documents to delete")
		return nil
	}

	// document.purged — mirrors DocumentMacroEvent::purged.
	for _, id := range docIDs {
		err := d.publishEvent(ctx, events.StreamDocuments, id, "document.purged", map[string]any{
			"document_id": id,
		})
		if err != nil {
			return fmt.Errorf("publish document purge event %s: %w", id, err)
		}
	}

	for _, id := range docIDs {
		if err := d.publishJob(ctx, subjDeleteDocument, "document.delete", id, map[string]any{
			"document_id": id,
		}); err != nil {
			return fmt.Errorf("enqueue document delete %s: %w", id, err)
		}
	}
	return nil
}

// deleteProjectsBulk ports macro_db_client::projects::delete::delete_projects_bulk:
// pins, history, share permissions, entity_access rows, entity registry rows,
// then the projects themselves — all in one transaction.
func deleteProjectsBulk(ctx context.Context, db *pgxpool.Pool, projectIDs []string) error {
	if len(projectIDs) == 0 {
		return nil
	}
	projectUUIDs, err := toUUIDs(projectIDs)
	if err != nil {
		return err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		DELETE FROM "Pin" WHERE "pinnedItemId" = ANY($1) AND "pinnedItemType" = 'project'
	`, projectIDs); err != nil {
		return fmt.Errorf("delete pins: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM "UserHistory" WHERE "itemId" = ANY($1) AND "itemType" = 'project'
	`, projectIDs); err != nil {
		return fmt.Errorf("delete user history: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM "SharePermission"
		WHERE id IN (
			SELECT "sharePermissionId"
			FROM "ProjectPermission"
			WHERE "projectId" = ANY($1)
		)
	`, projectIDs); err != nil {
		return fmt.Errorf("delete share permissions: %w", err)
	}

	// entity_access: project rows plus anything granted via these projects.
	if _, err := tx.Exec(ctx, `
		DELETE FROM "entity_access"
		WHERE (entity_id = ANY($1) AND entity_type = 'project')
		OR granted_from_project_id = ANY($2)
	`, projectUUIDs, projectIDs); err != nil {
		return fmt.Errorf("delete entity access: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM entity WHERE id = ANY($1)
	`, projectUUIDs); err != nil {
		return fmt.Errorf("delete entity rows: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM "Project" WHERE id = ANY($1)
	`, projectIDs); err != nil {
		return fmt.Errorf("delete projects: %w", err)
	}

	return tx.Commit(ctx)
}
