// Persistence helpers that have no generated sqlc query yet. These mirror the
// SQL in crates/chat/src/outbound/postgres/queries/* and the *_db_utils
// crates, kept inside the dcs package until matching sqlc queries are added.
package dcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// repo wraps the generated queries plus the pool for transactions and the
// small number of raw statements the sqlc file set doesn't cover.
type repo struct {
	pool *pgxpool.Pool
	q    *macrodb.Queries
}

func newRepo(pool *pgxpool.Pool) *repo {
	return &repo{pool: pool, q: macrodb.New(pool)}
}

// withTx runs fn inside a transaction, exposing a *macrodb.Queries bound to
// it plus the tx for raw statements.
func (r *repo) withTx(ctx context.Context, fn func(tx pgx.Tx, q *macrodb.Queries) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx, r.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// uuidOf parses a chat id into a pgtype.UUID (chats are uuid_v7).
func uuidOf(id string) (pgtype.UUID, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid uuid %q: %w", id, err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

// ts converts a pgtype timestamp to *time.Time.
func ts(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	tt := t.Time
	return &tt
}

// ---------------------------------------------------------------------------
// chat create support (mirrors create_chat_permission / upsert_user_history /
// upsert_item_last_accessed / insert_entity_access_row / insert_entity)
// ---------------------------------------------------------------------------

// sharePerm holds the resolved link-share fields for a new chat.
type sharePerm struct {
	linkShare *string
	linkLevel *string
}

// insertSharePermission creates the SharePermission + ChatPermission rows for
// a chat. Mirrors create_chat_permission.rs.
func insertSharePermission(ctx context.Context, tx pgx.Tx, chatID string, sp sharePerm) error {
	var linkShare, linkLevel any
	if sp.linkShare != nil {
		linkShare = *sp.linkShare
		if sp.linkLevel != nil {
			linkLevel = *sp.linkLevel
		} else {
			linkLevel = AccessLevelView // default-to-view parity
		}
	}
	var permissionID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO "SharePermission" ("linkShare", "linkShareAccessLevel", "createdAt", "updatedAt")
		VALUES ($1, $2, NOW(), NOW())
		RETURNING id
	`, linkShare, linkLevel).Scan(&permissionID); err != nil {
		return fmt.Errorf("insert share permission: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "ChatPermission" ("chatId", "sharePermissionId") VALUES ($1, $2)
	`, chatID, permissionID); err != nil {
		return fmt.Errorf("insert chat permission: %w", err)
	}
	return nil
}

// upsertUserHistory mirrors upsert_user_history.rs (generated query).
func upsertUserHistory(ctx context.Context, q *macrodb.Queries, userID, chatID string) error {
	return q.UpsertUserHistoryTimestamp(ctx, macrodb.UpsertUserHistoryTimestampParams{
		UserId:    userID,
		ItemId:    chatID,
		ItemType:  "chat",
		CreatedAt: pgtype.Timestamp{Time: time.Now(), Valid: true},
	})
}

// upsertItemLastAccessed mirrors upsert_item_last_accessed.rs (generated query).
func upsertItemLastAccessed(ctx context.Context, q *macrodb.Queries, chatID string) error {
	return q.UpsertItemLastAccessedTimestamp(ctx, macrodb.UpsertItemLastAccessedTimestampParams{
		ItemID:       chatID,
		ItemType:     "chat",
		LastAccessed: pgtype.Timestamp{Time: time.Now(), Valid: true},
	})
}

// removeChatPins mirrors the Pin cleanup in soft_delete_chat /
// permanently_delete_chat (generated query).
func removeChatPins(ctx context.Context, q *macrodb.Queries, chatID string) error {
	return q.RemovePinByPinnedItemId(ctx, macrodb.RemovePinByPinnedItemIdParams{
		PinnedItemId:   chatID,
		PinnedItemType: "chat",
	})
}

// insertOwnerEntityAccess mirrors entity_access_db_utils::insert_entity_access_row
// for the owner grant.
func insertOwnerEntityAccess(ctx context.Context, tx pgx.Tx, chatID, userID string) error {
	u, err := uuidOf(chatID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO entity_access (entity_id, entity_type, source_id, source_type, access_level)
		VALUES ($1, 'chat', $2, 'user', 'owner')
	`, u, userID)
	return err
}

// registerEntity mirrors entity_registry_db_utils::insert_entity (idempotent).
func registerEntity(ctx context.Context, tx pgx.Tx, chatID, ownerID string) error {
	u, err := uuidOf(chatID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO entity (id, entity_type, owner_type, owner_id, created_at, updated_at)
		VALUES ($1, 'chat', 'user', $2, now(), now())
		ON CONFLICT (id) DO NOTHING
	`, u, ownerID)
	return err
}

// markEntityDeleted / clearEntityDeleted / deleteEntity mirror the entity
// registry helpers.
func markEntityDeleted(ctx context.Context, tx pgx.Tx, chatID string) error {
	u, err := uuidOf(chatID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE entity SET deleted_at = now() WHERE id = $1`, u)
	return err
}

func clearEntityDeleted(ctx context.Context, tx pgx.Tx, chatID string) error {
	u, err := uuidOf(chatID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE entity SET deleted_at = NULL WHERE id = $1`, u)
	return err
}

func deleteEntityRow(ctx context.Context, tx pgx.Tx, chatID string) error {
	u, err := uuidOf(chatID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM entity WHERE id = $1`, u)
	return err
}

// teamDefaultLinkShare mirrors share_permission_db_utils::get_team_default_link_share.
// Returns ("", false) when the user has no team preference row.
func (r *repo) teamDefaultLinkShare(ctx context.Context, userID string) (linkShare *string, hasRow bool, err error) {
	var v *string
	err = r.pool.QueryRow(ctx, `
		SELECT t.default_link_share
		FROM team_user tu
		JOIN team t ON t.id = tu.team_id
		WHERE tu.user_id = $1
		ORDER BY tu.team_role DESC
		LIMIT 1
	`, userID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return v, true, err
}

// newChatSharePermission resolves the initial share permission for a new
// chat, mirroring SharePermissionV2::new_chat_share_permission: default
// PUBLIC/view unless the owner's team default says otherwise.
func (r *repo) newChatSharePermission(ctx context.Context, userID string) (sharePerm, error) {
	v, hasRow, err := r.teamDefaultLinkShare(ctx, userID)
	if err != nil {
		return sharePerm{}, err
	}
	switch {
	case !hasRow:
		pub, view := LinkSharePublic, AccessLevelView
		return sharePerm{linkShare: &pub, linkLevel: &view}, nil
	case v == nil:
		// Team explicitly turned link sharing off.
		return sharePerm{}, nil
	default:
		return sharePerm{linkShare: v, linkLevel: strptr(AccessLevelView)}, nil
	}
}

func strptr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// patch support
// ---------------------------------------------------------------------------

// touchChat bumps "updatedAt" (patch_chat.rs always bumps it first).
func touchChat(ctx context.Context, tx pgx.Tx, chatID string) error {
	_, err := tx.Exec(ctx, `UPDATE "Chat" SET "updatedAt" = NOW() WHERE id = $1`, chatID)
	return err
}

// updateSharePermission mirrors edit_share_permission's UPDATE on the
// SharePermission row, then channel share permissions.
func updateSharePermission(ctx context.Context, tx pgx.Tx, chatID string, req *UpdateSharePermissionRequestV2) error {
	var shareID string
	if err := tx.QueryRow(ctx, `
		SELECT cp."sharePermissionId" FROM "ChatPermission" cp WHERE cp."chatId" = $1
	`, chatID).Scan(&shareID); err != nil {
		return fmt.Errorf("find share permission: %w", err)
	}

	updateLinkShare := req.linkShareSet
	var linkShare any
	var updateLinkLevel bool
	var linkLevel any
	if req.linkShareSet {
		if req.linkShare != nil {
			linkShare = *req.linkShare
			lv := AccessLevelView
			if req.linkShareAccessLevel != nil {
				lv = *req.linkShareAccessLevel
			}
			updateLinkLevel, linkLevel = true, lv
		} else {
			updateLinkLevel = true
		}
	} else if req.linkShareLevelSet {
		updateLinkLevel = true
		if req.linkShareAccessLevel != nil {
			linkLevel = *req.linkShareAccessLevel
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE "SharePermission"
		SET
			"linkShare" = CASE WHEN $2 THEN $3 ELSE "linkShare" END,
			"linkShareAccessLevel" = CASE
				WHEN $2 AND $3 IS NULL THEN NULL
				WHEN $2 THEN COALESCE($5::"AccessLevel", 'view')
				WHEN $4 AND "linkShare" IS NOT NULL THEN COALESCE($5::"AccessLevel", 'view')
				WHEN $4 THEN NULL
				ELSE "linkShareAccessLevel"
			END,
			"updatedAt" = NOW()
		WHERE id = $1
	`, shareID, updateLinkShare, linkShare, updateLinkLevel, linkLevel); err != nil {
		return fmt.Errorf("update share permission: %w", err)
	}

	if req.channelSharePermsSet {
		if err := updateChannelSharePermissions(ctx, tx, shareID, chatID, req.ChannelSharePerms); err != nil {
			return err
		}
	}
	return nil
}

// updateChannelSharePermissions mirrors edit_channel_share_permissions plus
// the chat branch of update_entity_access_channel_share_permissions.
func updateChannelSharePermissions(ctx context.Context, tx pgx.Tx, sharePermissionID, chatID string, perms []UpdateChannelSharePermission) error {
	var remove []string
	type upsert struct {
		channelID string
		level     string
	}
	var ups []upsert
	for _, p := range perms {
		switch p.Operation {
		case "add", "replace":
			level := AccessLevelView
			if p.AccessLevel != nil {
				level = *p.AccessLevel
			}
			ups = append(ups, upsert{channelID: p.ChannelID, level: level})
		case "remove":
			remove = append(remove, p.ChannelID)
		default:
			return fmt.Errorf("unknown channel share operation %q", p.Operation)
		}
	}

	if len(remove) > 0 {
		if _, err := tx.Exec(ctx, `
			DELETE FROM "ChannelSharePermission"
			WHERE "share_permission_id" = $1 AND "channel_id" = ANY($2)
		`, sharePermissionID, remove); err != nil {
			return fmt.Errorf("delete channel share permissions: %w", err)
		}
		u, err := uuidOf(chatID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM entity_access
			WHERE entity_id = $1 AND entity_type = 'chat'
			AND source_id = ANY($2) AND source_type = 'channel'
		`, u, remove); err != nil {
			return fmt.Errorf("delete channel entity access: %w", err)
		}
	}

	if len(ups) > 0 {
		channelIDs := make([]string, len(ups))
		levels := make([]string, len(ups))
		for i, u := range ups {
			channelIDs[i] = u.channelID
			levels[i] = u.level
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO "ChannelSharePermission" ("share_permission_id", "channel_id", "access_level")
			SELECT $1, channel_id, access_level::"AccessLevel"
			FROM UNNEST($2::text[], $3::text[]) AS t(channel_id, access_level)
			ON CONFLICT ("share_permission_id", "channel_id")
			DO UPDATE SET "access_level" = EXCLUDED."access_level"
		`, sharePermissionID, channelIDs, levels); err != nil {
			return fmt.Errorf("upsert channel share permissions: %w", err)
		}

		u, err := uuidOf(chatID)
		if err != nil {
			return err
		}
		// entity_access upsert for the chat itself (chat branch of
		// update_entity_access_channel_share_permissions).
		if _, err := tx.Exec(ctx, `
			INSERT INTO entity_access (entity_id, entity_type, source_id, source_type, access_level)
			SELECT $1, 'chat', channel_id, 'channel', access_level::"AccessLevel"
			FROM UNNEST($2::text[], $3::text[]) AS t(channel_id, access_level)
			ON CONFLICT (entity_id, entity_type, source_id, source_type)
			WHERE granted_from_project_id IS NULL
			DO UPDATE SET access_level = EXCLUDED.access_level, updated_at = NOW()
		`, u, channelIDs, levels); err != nil {
			return fmt.Errorf("upsert channel entity access: %w", err)
		}
	}
	return nil
}

// insertChat mirrors queries::insert_chat (chat-crate create leaves model at
// its 'gpt-4o' column default and isPersistent=false).
func insertChat(ctx context.Context, tx pgx.Tx, userID, name string, projectID *string) (string, error) {
	var project any
	if projectID != nil && *projectID != "" {
		project = *projectID
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO "Chat" ("userId", name, "projectId")
		VALUES ($1, $2, $3)
		RETURNING id
	`, userID, name, project).Scan(&id)
	return id, err
}

// insertChatV2 mirrors macro_db_client::dcs::create_chat_v2's insert (the DCS
// stream path persists the model + is_persistent).
func insertChatV2(ctx context.Context, tx pgx.Tx, q *macrodb.Queries, userID, name, model string, projectID *string, isPersistent bool) (string, error) {
	var proj pgtype.Text
	if projectID != nil && *projectID != "" {
		proj = pgtype.Text{String: *projectID, Valid: true}
	}
	row, err := q.CreateChatV2(ctx, macrodb.CreateChatV2Params{
		UserId:       userID,
		Name:         name,
		Model:        model,
		ProjectId:    proj,
		IsPersistent: isPersistent,
	})
	return row.ID, err
}

// updateProjectModified mirrors update_project_modified.rs.
func (r *repo) updateProjectModified(ctx context.Context, projectID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE "Project" SET "updatedAt" = NOW() WHERE id = $1`, projectID)
	return err
}

// ---------------------------------------------------------------------------
// message support
// ---------------------------------------------------------------------------

// newMessage mirrors model::chat::NewChatMessage.
type newMessage struct {
	id          string // "" → generated
	content     MessageContent
	role        string
	model       string
	attachments []newAttachment // only persisted for user messages
	createdAt   time.Time
}

// newAttachment mirrors model::chat::NewAttachment (attachment_type +
// attachment_id). attachment entity types are stored as the EntityType the
// Rust attachment_type_to_entity_type maps to.
type newAttachment struct {
	entityType string
	entityID   string
}

// createMessage mirrors queries::create_message.rs: insert + attachments
// (user role only) + bump chat updatedAt / model (user role only).
func (r *repo) createMessage(ctx context.Context, chatID string, msg newMessage) (string, error) {
	id := msg.id
	if id == "" {
		id = uuid.NewString()
	}
	err := r.withTx(ctx, func(tx pgx.Tx, q *macrodb.Queries) error {
		now := pgtype.Timestamp{Time: msg.createdAt, Valid: true}
		var model pgtype.Text
		if msg.model != "" {
			model = pgtype.Text{String: msg.model, Valid: true}
		}
		messageID, err := q.CreateChatMessage(ctx, macrodb.CreateChatMessageParams{
			ID:        id,
			ChatId:    chatID,
			Content:   []byte(msg.content.Raw()),
			Role:      msg.role,
			Model:     model,
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("insert chat message: %w", err)
		}
		if msg.role == RoleUser && len(msg.attachments) > 0 {
			kinds := make([]string, len(msg.attachments))
			ids := make([]pgtype.UUID, len(msg.attachments))
			chatIDs := make([]string, len(msg.attachments))
			msgIDs := make([]string, len(msg.attachments))
			for i, a := range msg.attachments {
				u, err := uuid.Parse(a.entityID)
				if err != nil {
					return fmt.Errorf("invalid attachment id %q: %w", a.entityID, err)
				}
				kinds[i] = a.entityType
				ids[i] = pgtype.UUID{Bytes: u, Valid: true}
				chatIDs[i] = chatID
				msgIDs[i] = messageID
			}
			if err := q.CreateChatMessage2(ctx, macrodb.CreateChatMessage2Params{
				Column1: kinds,
				Column2: ids,
				Column3: chatIDs,
				Column4: msgIDs,
			}); err != nil {
				return fmt.Errorf("insert chat attachments: %w", err)
			}
		}
		// Bump chat recency; a user message also records the model used.
		var selectedModel any
		if msg.role == RoleUser && msg.model != "" {
			selectedModel = msg.model
		}
		if _, err := tx.Exec(ctx, `
			UPDATE "Chat" SET "updatedAt" = NOW(), model = COALESCE($2, model) WHERE id = $1
		`, chatID, selectedModel); err != nil {
			return fmt.Errorf("bump chat recency: %w", err)
		}
		return nil
	})
	return id, err
}

// chatHistoryRow mirrors macro_db_client::chat_history::ChatHistoryRow. Raw
// SQL is used instead of the generated GetChatHistory* queries because sqlc
// typed attachment_id as non-nullable string while the LEFT JOIN yields NULL.
type chatHistoryRow struct {
	chatID       string
	chatTitle    string
	content      string
	createdAt    time.Time
	attachmentID *string
}

func scanChatHistoryRows(rows pgx.Rows) ([]chatHistoryRow, error) {
	defer rows.Close()
	var out []chatHistoryRow
	for rows.Next() {
		var r chatHistoryRow
		var att pgtype.Text
		var t pgtype.Timestamp
		if err := rows.Scan(&r.chatID, &r.chatTitle, &r.content, &t, &att); err != nil {
			return nil, err
		}
		r.createdAt = t.Time
		if att.Valid {
			r.attachmentID = &att.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (r *repo) chatHistory(ctx context.Context, chatID string) ([]chatHistoryRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.id as chat_id, c.name as chat_title,
			m.content::text as message_content, m."createdAt" as message_created_at,
			ma.entity_id::text as attachment_id
		FROM "Chat" c
		JOIN "ChatMessage" m ON m."chatId" = c.id
		LEFT JOIN "ChatAttachment" ma ON ma."messageId" = m.id
		WHERE c.id = $1
		ORDER BY m."createdAt" ASC
	`, chatID)
	if err != nil {
		return nil, err
	}
	return scanChatHistoryRows(rows)
}

func (r *repo) chatHistoryForMessages(ctx context.Context, messageIDs []string) ([]chatHistoryRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.id as chat_id, c.name as chat_title,
			m.content::text as message_content, m."createdAt" as message_created_at,
			ma.entity_id::text as attachment_id
		FROM "Chat" c
		JOIN "ChatMessage" m ON m."chatId" = c.id
		LEFT JOIN "ChatAttachment" ma ON ma."messageId" = m.id
		WHERE m."id" = ANY($1::text[])
		ORDER BY m."createdAt" ASC
	`, messageIDs)
	if err != nil {
		return nil, err
	}
	return scanChatHistoryRows(rows)
}

// attachmentChats mirrors get_latest_single_attachment_chat +
// get_multi_attachment_chat. Raw SQL because the generated queries type $1 as
// uuid while the comparison is entity_id::TEXT = $1 (Rust binds a string).
func (r *repo) attachmentChats(ctx context.Context, attachmentID, userID string) (recent *Chat, all []Chat, err error) {
	err = r.withTx(ctx, func(tx pgx.Tx, _ *macrodb.Queries) error {
		row := tx.QueryRow(ctx, `
			WITH SingleAttachmentChats AS (
				SELECT "chatId", COUNT(*) as attachment_count
				FROM "ChatAttachment" GROUP BY "chatId" HAVING COUNT(*) = 1
			)
			SELECT c.id, c.name, c."userId", c."createdAt"::timestamptz, c."updatedAt"::timestamptz,
				c."deletedAt"::timestamptz, c.model, c."tokenCount", c."projectId", c."isPersistent"
			FROM "Chat" c
			INNER JOIN "ChatAttachment" ca ON c.id = ca."chatId"
			INNER JOIN SingleAttachmentChats sac ON c.id = sac."chatId"
			WHERE ca."entity_id"::TEXT = $1 AND c."userId" = $2 AND c."deletedAt" IS NULL
			ORDER BY c."updatedAt" DESC LIMIT 1
		`, attachmentID, userID)
		c, err := scanAttachmentChat(row)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		recent = c

		rows, err := tx.Query(ctx, `
			WITH MultiAttachmentChats AS (
				SELECT "chatId", COUNT(*) as attachment_count
				FROM "ChatAttachment" GROUP BY "chatId"
			)
			SELECT c.id, c.name, c."userId", c."createdAt"::timestamptz, c."updatedAt"::timestamptz,
				c."deletedAt"::timestamptz, c.model, c."tokenCount", c."projectId", c."isPersistent"
			FROM "Chat" c
			INNER JOIN "ChatAttachment" ca ON c.id = ca."chatId"
			INNER JOIN MultiAttachmentChats mac ON c.id = mac."chatId"
			WHERE ca."entity_id"::TEXT = $1 AND c."userId" = $2 AND c."deletedAt" IS NULL
			ORDER BY c."updatedAt" DESC
		`, attachmentID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanAttachmentChat(rows)
			if err != nil {
				return err
			}
			all = append(all, *c)
		}
		return rows.Err()
	})
	return recent, all, err
}

func scanAttachmentChat(row pgx.Row) (*Chat, error) {
	var c Chat
	var createdAt, updatedAt, deletedAt pgtype.Timestamptz
	var model string
	var tokenCount pgtype.Int8
	var projectID pgtype.Text
	if err := row.Scan(&c.ID, &c.Name, &c.UserID, &createdAt, &updatedAt, &deletedAt,
		&model, &tokenCount, &projectID, &c.IsPersistent); err != nil {
		return nil, err
	}
	c.CreatedAt = ts(createdAt)
	c.UpdatedAt = ts(updatedAt)
	c.DeletedAt = ts(deletedAt)
	c.Model = optionalString(model)
	c.TokenCount = int8Ptr(tokenCount)
	c.ProjectID = textPtr(projectID)
	return &c, nil
}

// userAccessLevel mirrors queries::get_access_level.rs — entity_access rows
// reachable through the user's source ids (channels, teams, direct). Defaults
// to "view" when no row exists (matching the Rust unwrap_or_default).
func (r *repo) userAccessLevel(ctx context.Context, userID, chatID string) (string, error) {
	u, err := uuidOf(chatID)
	if err != nil {
		return "", err
	}
	var level *string
	err = r.pool.QueryRow(ctx, `
		SELECT access_level::text
		FROM entity_access
		WHERE source_id = ANY(ARRAY(
			SELECT cp.channel_id::text FROM comms_channel_participants cp
				WHERE cp.user_id = $1 AND cp.left_at IS NULL
			UNION ALL
			SELECT t.team_id::text FROM team_user t WHERE t.user_id = $1
			UNION ALL
			SELECT $1
		))
		AND entity_id = $2 AND entity_type = 'chat'
		ORDER BY CASE access_level::text
			WHEN 'owner' THEN 4 WHEN 'edit' THEN 3 WHEN 'comment' THEN 2 WHEN 'view' THEN 1
			ELSE 0 END DESC
		LIMIT 1
	`, userID, u).Scan(&level)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if level == nil || *level == "" {
		return AccessLevelView, nil
	}
	return *level, nil
}

// updateMessageContent mirrors update_message_content.rs.
func (r *repo) updateMessageContent(ctx context.Context, chatID, messageID string, content MessageContent, bumpChatRecency bool) error {
	return r.withTx(ctx, func(tx pgx.Tx, _ *macrodb.Queries) error {
		tag, err := tx.Exec(ctx, `
			UPDATE "ChatMessage" SET "content" = $1, "updatedAt" = NOW()
			WHERE "id" = $2 AND "chatId" = $3
		`, []byte(content.Raw()), messageID, chatID)
		if err != nil {
			return err
		}
		if bumpChatRecency && tag.RowsAffected() > 0 {
			return touchChat(ctx, tx, chatID)
		}
		return nil
	})
}

// getMessageContent mirrors get_message_content.rs.
func (r *repo) getMessageContent(ctx context.Context, chatID, messageID string) (MessageContent, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `
		SELECT "content" FROM "ChatMessage" WHERE "id" = $1 AND "chatId" = $2
	`, messageID, chatID).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MessageContent{}, errf(errNotFound, "message not found")
		}
		return MessageContent{}, err
	}
	return MessageContent{raw: json.RawMessage(raw)}, nil
}

// storeResolvedMessage mirrors store_resolved_message.rs. `parts` is the
// resolver output JSON (attachment::FormattedParts).
func (r *repo) storeResolvedMessage(ctx context.Context, messageID string, parts json.RawMessage) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO resolved_message_content ("messageId", "content")
		VALUES ($1, $2)
		ON CONFLICT ("messageId") DO UPDATE SET "content" = $2
	`, messageID, parts)
	return err
}

// getResolvedMessage mirrors get_resolved_message.rs; returns nil, nil when
// no resolved content exists.
func (r *repo) getResolvedMessage(ctx context.Context, messageID string) (json.RawMessage, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `
		SELECT "content" FROM resolved_message_content WHERE "messageId" = $1
	`, messageID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

// getResolvedMessageChain returns resolved content for every message of a
// chat that has any (mirrors MessageService::get_resolved_message_chain).
func (r *repo) getResolvedMessageChain(ctx context.Context, chatID string) ([]json.RawMessage, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT rmc."content"
		FROM resolved_message_content rmc
		JOIN "ChatMessage" cm ON cm.id = rmc."messageId"
		WHERE cm."chatId" = $1 AND cm."role" = 'user'
		ORDER BY cm."createdAt" ASC
	`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// misc lookups
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// project membership (mirrors entity_access_management add/remove_entity_to_project)
// ---------------------------------------------------------------------------

// walkUpProjectTree mirrors entity_access_db_utils::walk_up_project_tree.
func walkUpProjectTree(ctx context.Context, tx pgx.Tx, projectID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE parent_projects AS (
			SELECT id, name, "parentId" FROM "Project" WHERE id = $1
			UNION ALL
			SELECT p.id, p.name, p."parentId" FROM "Project" p
			INNER JOIN parent_projects pp ON p.id = pp."parentId"
		)
		SELECT id FROM parent_projects
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// addEntityToProject grants the chat project-inherited access rows from every
// project in the tree above projectID.
func (r *repo) addEntityToProject(ctx context.Context, chatID, projectID string) error {
	return r.withTx(ctx, func(tx pgx.Tx, _ *macrodb.Queries) error {
		projectIDs, err := walkUpProjectTree(ctx, tx, projectID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT entity_id, source_id, source_type, access_level
			FROM entity_access
			WHERE entity_id = ANY($1::uuid[]) AND entity_type = 'project' AND granted_from_project_id IS NULL
		`, projectIDs)
		if err != nil {
			return err
		}
		type src struct {
			projectID  string
			sourceID   string
			sourceType string
			level      string
		}
		var sources []src
		for rows.Next() {
			var s src
			var entityID pgtype.UUID
			if err := rows.Scan(&entityID, &s.sourceID, &s.sourceType, &s.level); err != nil {
				rows.Close()
				return err
			}
			s.projectID = uuid.UUID(entityID.Bytes).String()
			sources = append(sources, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(sources) == 0 {
			return nil
		}
		chatUUID, err := uuidOf(chatID)
		if err != nil {
			return err
		}
		for _, s := range sources {
			if _, err := tx.Exec(ctx, `
				INSERT INTO entity_access (entity_id, entity_type, source_id, source_type, access_level, granted_from_project_id)
				VALUES ($1, 'chat', $2, $3, $4, $5)
				ON CONFLICT DO NOTHING
			`, chatUUID, s.sourceID, s.sourceType, s.level, s.projectID); err != nil {
				return err
			}
		}
		return nil
	})
}

// removeEntityFromProject drops the chat's project-inherited access rows.
func (r *repo) removeEntityFromProject(ctx context.Context, chatID, projectID string) error {
	return r.withTx(ctx, func(tx pgx.Tx, _ *macrodb.Queries) error {
		projectIDs, err := walkUpProjectTree(ctx, tx, projectID)
		if err != nil {
			return err
		}
		if len(projectIDs) == 0 {
			return nil
		}
		chatUUID, err := uuidOf(chatID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			DELETE FROM entity_access
			WHERE entity_id = $1 AND entity_type = 'chat'
			AND granted_from_project_id = ANY($2)
		`, chatUUID, projectIDs)
		return err
	})
}

// createIDMapping / getIDMapping mirror service::id_mapping.
func (r *repo) createIDMapping(ctx context.Context, sourceID, targetID string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO id_mapping (source_id, target_id)
		VALUES ($1, $2)
		ON CONFLICT (source_id) DO UPDATE SET target_id = $2
	`, sourceID, targetID)
	return err
}

func (r *repo) getIDMapping(ctx context.Context, sourceID string) (*string, error) {
	var v string
	err := r.pool.QueryRow(ctx, `
		SELECT target_id FROM id_mapping WHERE source_id = $1
	`, sourceID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}
