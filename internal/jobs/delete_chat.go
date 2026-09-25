package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macro-inc/macro/pkg/events"
)

// deleteChatJob is the envelope payload for subjDeleteChat. It carries the
// SQS message attribute the Rust handler read; producers (deleted_item_poll,
// dss soft-delete paths) publish one message per chat.
type deleteChatJob struct {
	ChatID string `json:"chat_id"`
}

// handleDeleteChat ports services/delete_chat_handler: hard-delete one chat
// row and its referential debris, but only while the row is still soft-
// deleted (a reverted/undeleted chat must survive its queued delete).
func (d *deps) handleDeleteChat(ctx context.Context, env events.Envelope) error {
	var job deleteChatJob
	if err := json.Unmarshal(env.Data, &job); err != nil {
		return fmt.Errorf("decode delete_chat job: %w", err)
	}
	if job.ChatID == "" {
		return errors.New("delete_chat job missing chat_id")
	}
	log := slog.With("chat_id", job.ChatID)

	deleted, err := d.isChatDeleted(ctx, job.ChatID)
	if err != nil {
		return fmt.Errorf("check chat deleted status: %w", err)
	}
	if !deleted {
		// Either the row is gone (a previous delivery already deleted it)
		// or it was reverted — both are terminal, matching the Rust skip.
		log.Info("delete_chat: chat is not deleted, skipping")
		return nil
	}

	if err := deleteChatTx(ctx, d.pools.MacroDB, job.ChatID); err != nil {
		return fmt.Errorf("delete chat %s: %w", job.ChatID, err)
	}
	log.Info("delete_chat: chat deleted")
	return nil
}

// isChatDeleted ports delete_chat_handler's is_chat_deleted: the row's
// deletedAt must be set. A missing row counts as "not deleted" — the delete
// already happened.
func (d *deps) isChatDeleted(ctx context.Context, chatID string) (bool, error) {
	var deletedAt *time.Time
	err := d.pools.MacroDB.QueryRow(ctx, `
		SELECT "deletedAt" FROM "Chat" WHERE id = $1
	`, chatID).Scan(&deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return deletedAt != nil, nil
}

// deleteChatTx ports delete_chat_handler's delete_chat: pins, history,
// share permission (via ChatPermission), entity_access rows, the chat, then
// the entity registry row — all in one transaction.
func deleteChatTx(ctx context.Context, db *pgxpool.Pool, chatID string) error {
	chatUUID, err := uuid.Parse(chatID)
	if err != nil {
		return fmt.Errorf("parse chat uuid %q: %w", chatID, err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		DELETE FROM "Pin" WHERE "pinnedItemId" = $1 AND "pinnedItemType" = 'chat'
	`, chatID); err != nil {
		return fmt.Errorf("delete pins: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM "UserHistory" WHERE "itemId" = $1 AND "itemType" = 'chat'
	`, chatID); err != nil {
		return fmt.Errorf("delete user history: %w", err)
	}

	var sharePermissionID *string
	err = tx.QueryRow(ctx, `
		SELECT "sharePermissionId" FROM "ChatPermission" WHERE "chatId" = $1
	`, chatID).Scan(&sharePermissionID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get chat permission: %w", err)
	}
	if sharePermissionID != nil {
		if _, err := tx.Exec(ctx, `
			DELETE FROM "SharePermission" WHERE id = $1
		`, *sharePermissionID); err != nil {
			return fmt.Errorf("delete share permission: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM entity_access WHERE entity_id = $1 AND entity_type = 'chat'
	`, pgtype.UUID{Bytes: chatUUID, Valid: true}); err != nil {
		return fmt.Errorf("delete entity access: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM "Chat" WHERE id = $1
	`, chatID); err != nil {
		return fmt.Errorf("delete chat row: %w", err)
	}

	// entity_registry_db_utils::delete_entity
	if _, err := tx.Exec(ctx, `
		DELETE FROM entity WHERE id = $1
	`, pgtype.UUID{Bytes: chatUUID, Valid: true}); err != nil {
		return fmt.Errorf("delete entity row: %w", err)
	}

	return tx.Commit(ctx)
}
