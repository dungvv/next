// Chat CRUD handlers — the Go port of crates/chat's inbound http router plus
// the DCS history endpoints. Lifecycle events on macro.chats.<chat_id> mirror
// ChatMacroEvent::* (sanitized, content-free).
package dcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// defaultChatName mirrors core::constants::DEFAULT_CHAT_NAME.
const defaultChatName = "New Chat"

// maxChatNameLen mirrors the 100-grapheme cap in ChatServiceImpl (approximated
// by runes).
const maxChatNameLen = 100

// eventActor mirrors event_actor_user_id: nil for unauthenticated requests and
// for internal callers without a forwarded user.
func eventActor(c auth.Caller, authed bool) *string {
	if !authed {
		return nil
	}
	if c.Internal && c.UserID == auth.InternalUserID {
		return nil
	}
	uid := c.UserID
	return &uid
}

// isUserOwner mirrors model_owner::Owner::from_principal_str: "macro|" →
// user, "bot|" → bot, anything else → team.
func isUserOwner(principal string) bool { return strings.HasPrefix(principal, "macro|") }

// ---------------------------------------------------------------------------
// domain operations
// ---------------------------------------------------------------------------

// createChat performs the chat-crate create transaction (insert_chat +
// create_chat_permission + upsert_user_history + upsert_item_last_accessed +
// entity_access owner grant + entity registry insert) and publishes
// chat.created.
func (s *Service) createChat(ctx context.Context, userID, name string, projectID *string) (string, error) {
	if utf8.RuneCountInString(name) > maxChatNameLen {
		return "", errf(errBadRequest, "name too long")
	}
	sp, err := s.repo.newChatSharePermission(ctx, userID)
	if err != nil {
		return "", wrapErr(errInternal, "resolve team default link share", err)
	}

	var chatID string
	err = s.repo.withTx(ctx, func(tx pgx.Tx, q *macrodb.Queries) error {
		id, err := insertChat(ctx, tx, userID, name, projectID)
		if err != nil {
			return fmt.Errorf("insert chat: %w", err)
		}
		chatID = id
		return s.createChatTxTail(ctx, tx, q, chatID, userID, sp)
	})
	if err != nil {
		return "", wrapErr(errInternal, "create chat", err)
	}
	s.afterChatCreate(ctx, chatID, userID, name, projectID)
	return chatID, nil
}

// createChatTxTail runs the post-insert steps shared by create and copy.
func (s *Service) createChatTxTail(ctx context.Context, tx pgx.Tx, q *macrodb.Queries, chatID, userID string, sp sharePerm) error {
	if err := insertSharePermission(ctx, tx, chatID, sp); err != nil {
		return err
	}
	if err := upsertUserHistory(ctx, q, userID, chatID); err != nil {
		return fmt.Errorf("upsert user history: %w", err)
	}
	if err := upsertItemLastAccessed(ctx, q, chatID); err != nil {
		return fmt.Errorf("upsert last accessed: %w", err)
	}
	if err := insertOwnerEntityAccess(ctx, tx, chatID, userID); err != nil {
		return fmt.Errorf("insert entity access: %w", err)
	}
	if err := registerEntity(ctx, tx, chatID, userID); err != nil {
		return fmt.Errorf("register entity: %w", err)
	}
	return nil
}

// afterChatCreate runs the post-commit fixups (project membership + modified
// bump) and publishes chat.created.
func (s *Service) afterChatCreate(ctx context.Context, chatID, userID, name string, projectID *string) {
	if projectID != nil && *projectID != "" &&
		isUUID(chatID) && isUUID(*projectID) {
		if err := s.repo.addEntityToProject(ctx, chatID, *projectID); err != nil {
			// Rust logs and continues (entity_access_management errors are
			// non-fatal to the create).
			slog.Warn("dcs: add entity to project", "err", err, "chat", chatID, "project", *projectID)
		}
		if err := s.repo.updateProjectModified(ctx, *projectID); err != nil {
			slog.Warn("dcs: update project modified", "err", err, "project", *projectID)
		}
	}
	s.events.publish(ctx, chatID, EventChatCreated, chatCreatedMeta{
		ChatID:    chatID,
		Owner:     userID,
		Name:      name,
		ProjectID: projectID,
	})
}

// createStreamChat mirrors the DCS stream path's create_new_chat: the
// create_chat_v2 insert (model + is_persistent persisted) + the same
// permission/history/access/entity tail.
func (s *Service) createStreamChat(ctx context.Context, userID, model string) (string, error) {
	sp, err := s.repo.newChatSharePermission(ctx, userID)
	if err != nil {
		return "", wrapErr(errInternal, "resolve team default link share", err)
	}
	var chatID string
	err = s.repo.withTx(ctx, func(tx pgx.Tx, q *macrodb.Queries) error {
		id, err := insertChatV2(ctx, tx, q, userID, defaultChatName, model, nil, true)
		if err != nil {
			return fmt.Errorf("insert chat: %w", err)
		}
		chatID = id
		return s.createChatTxTail(ctx, tx, q, chatID, userID, sp)
	})
	if err != nil {
		return "", wrapErr(errInternal, "create chat", err)
	}
	s.events.publish(ctx, chatID, EventChatCreated, chatCreatedMeta{
		ChatID: chatID,
		Owner:  userID,
		Name:   defaultChatName,
	})
	return chatID, nil
}

// getChatResponse builds the GetChatResponse for a chat (mirrors
// ChatServiceImpl::get_chat).
func (s *Service) getChatResponse(ctx context.Context, info *chatAccessInfo, chatID string) (*GetChatResponse, error) {
	meta, err := s.q.GetChatDb(ctx, chatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errf(errNotFound, "chat not found")
		}
		return nil, wrapErr(errInternal, "get chat", err)
	}
	msgs, err := s.q.GetMessages(ctx, chatID)
	if err != nil {
		return nil, wrapErr(errInternal, "get messages", err)
	}

	messages := make([]ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		var attachments []Entity
		if len(m.Attachments) > 0 {
			if err := json.Unmarshal(m.Attachments, &attachments); err != nil {
				return nil, wrapErr(errInternal, "decode attachments", err)
			}
		}
		if attachments == nil {
			attachments = []Entity{}
		}
		messages = append(messages, ChatMessage{
			ID:          m.ID,
			Content:     MessageContent{raw: json.RawMessage(m.Content)},
			Role:        m.Role,
			Attachments: attachments,
		})
	}

	// Rust reports the entity_access-derived level for authenticated users
	// (get_access_level defaults to view) and the receipt's granted level for
	// anonymous public-link viewers.
	level := info.level
	if level == "" {
		level = AccessLevelView
	}
	if info.userID != "" && !info.internal {
		if l, err := s.repo.userAccessLevel(ctx, info.userID, chatID); err == nil {
			level = l
		}
	}
	return &GetChatResponse{
		Chat: ChatResponse{
			ID:        meta.ID,
			UserID:    meta.UserID,
			ProjectID: textPtr(meta.ProjectID),
			Name:      meta.Name,
			Messages:  messages,
			Model:     optionalString(meta.Model),
			CreatedAt: meta.CreatedAt.Time,
			UpdatedAt: meta.UpdatedAt.Time,
		},
		UserAccessLevel: level,
	}, nil
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// softDelete mirrors ChatServiceImpl::delete (Pin + UserHistory cleanup,
// deletedAt stamp, entity registry mark) and publishes chat.deleted.
func (s *Service) softDelete(ctx context.Context, chatID string, actor *string) error {
	projectID, err := s.chatProjectID(ctx, chatID)
	if err != nil {
		return err
	}
	err = s.repo.withTx(ctx, func(tx pgx.Tx, q *macrodb.Queries) error {
		if err := removeChatPins(ctx, q, chatID); err != nil {
			return err
		}
		if err := q.SoftDeleteChat(ctx, macrodb.SoftDeleteChatParams{ItemId: chatID, ItemType: "chat"}); err != nil {
			return err
		}
		if err := q.SoftDeleteChat2(ctx, chatID); err != nil {
			return err
		}
		return markEntityDeleted(ctx, tx, chatID)
	})
	if err != nil {
		return wrapErr(errInternal, "delete chat", err)
	}
	s.afterProjectRemoval(ctx, chatID, projectID)
	s.events.publish(ctx, chatID, EventChatDeleted, chatDeletedMeta{
		ChatID:      chatID,
		ActorUserID: actor,
		ProjectID:   projectID,
	})
	return nil
}

// permanentlyDelete mirrors ChatServiceImpl::permanently_delete and publishes
// chat.permanently_deleted.
func (s *Service) permanentlyDelete(ctx context.Context, chatID string, actor *string) error {
	projectID, err := s.chatProjectID(ctx, chatID)
	if err != nil {
		return err
	}
	err = s.repo.withTx(ctx, func(tx pgx.Tx, q *macrodb.Queries) error {
		if err := removeChatPins(ctx, q, chatID); err != nil {
			return err
		}
		if err := q.SoftDeleteChat(ctx, macrodb.SoftDeleteChatParams{ItemId: chatID, ItemType: "chat"}); err != nil {
			return err
		}
		// Rust deletes SharePermission via a ChatPermission subquery.
		if _, err := tx.Exec(ctx, `
			DELETE FROM "SharePermission"
			WHERE id IN (SELECT "sharePermissionId" FROM "ChatPermission" WHERE "chatId" = $1)
		`, chatID); err != nil {
			return err
		}
		chatUUID, err := uuidOf(chatID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM entity_access WHERE entity_id = $1 AND entity_type = 'chat'`, chatUUID); err != nil {
			return err
		}
		if err := q.DeleteChat3(ctx, chatID); err != nil {
			return err
		}
		return deleteEntityRow(ctx, tx, chatID)
	})
	if err != nil {
		return wrapErr(errInternal, "permanently delete chat", err)
	}
	s.afterProjectRemoval(ctx, chatID, projectID)
	s.events.publish(ctx, chatID, EventChatPermanentlyDeleted, chatPermanentlyDeletedMeta{
		ChatID:      chatID,
		ActorUserID: actor,
		ProjectID:   projectID,
	})
	return nil
}

// chatProjectID fetches the current project id (for project-modified bumps
// and event metadata). Missing chat → nil.
func (s *Service) chatProjectID(ctx context.Context, chatID string) (*string, error) {
	meta, err := s.q.GetBasicChat(ctx, chatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, wrapErr(errInternal, "get chat metadata", err)
	}
	return textPtr(meta.ProjectID), nil
}

// afterProjectRemoval mirrors the project-detach side effects of delete /
// permanently_delete (remove entity_access grants, bump project timestamp).
func (s *Service) afterProjectRemoval(ctx context.Context, chatID string, projectID *string) {
	if projectID == nil || *projectID == "" || !isUUID(chatID) || !isUUID(*projectID) {
		return
	}
	if err := s.repo.removeEntityFromProject(ctx, chatID, *projectID); err != nil {
		slog.Warn("dcs: remove entity from project", "err", err, "chat", chatID, "project", *projectID)
	}
	if err := s.repo.updateProjectModified(ctx, *projectID); err != nil {
		slog.Warn("dcs: update project modified", "err", err, "project", *projectID)
	}
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// restoreChat mirrors ChatServiceImpl::revert_delete and publishes
// chat.restored.
func (s *Service) restoreChat(ctx context.Context, chatID string, actor *string) error {
	meta, err := s.q.GetBasicChat(ctx, chatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errf(errNotFound, "chat not found")
		}
		return wrapErr(errInternal, "get chat", err)
	}

	err = s.repo.withTx(ctx, func(tx pgx.Tx, q *macrodb.Queries) error {
		owner, err := q.RevertDeleteChat(ctx, chatID)
		if err != nil {
			return fmt.Errorf("revert delete: %w", err)
		}
		if err := clearEntityDeleted(ctx, tx, chatID); err != nil {
			return err
		}
		// Only a user has a history feed; bot- and team-owned chats skip it.
		if isUserOwner(owner) {
			if err := q.RevertDeleteChat2(ctx, macrodb.RevertDeleteChat2Params{
				UserId:   owner,
				ItemId:   chatID,
				ItemType: "chat",
			}); err != nil {
				return err
			}
		}
		if meta.ProjectID.Valid && meta.ProjectID.String != "" {
			deleted, err := q.RevertDeleteChat3(ctx, meta.ProjectID.String)
			if err != nil {
				return fmt.Errorf("check project deleted: %w", err)
			}
			if deleted.Valid {
				return q.RevertDeleteChat4(ctx, chatID)
			}
		}
		return nil
	})
	if err != nil {
		return wrapErr(errInternal, "restore chat", err)
	}
	s.events.publish(ctx, chatID, EventChatRestored, chatRestoredMeta{
		ChatID:      chatID,
		ActorUserID: actor,
		ProjectID:   textPtr(meta.ProjectID),
	})
	return nil
}

// copyChat mirrors ChatServiceImpl::copy_chat: new chat named "<src> Copy",
// same messages, fresh owner grants; publishes chat.copied.
func (s *Service) copyChat(ctx context.Context, userID, sourceChatID string) (string, error) {
	src, err := s.q.GetBasicChat(ctx, sourceChatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errf(errNotFound, "chat not found")
		}
		return "", wrapErr(errInternal, "get chat", err)
	}
	name := src.Name + " Copy"

	sp, err := s.repo.newChatSharePermission(ctx, userID)
	if err != nil {
		return "", wrapErr(errInternal, "resolve team default link share", err)
	}

	var chatID string
	err = s.repo.withTx(ctx, func(tx pgx.Tx, q *macrodb.Queries) error {
		id, err := insertChat(ctx, tx, userID, name, nil)
		if err != nil {
			return fmt.Errorf("insert chat: %w", err)
		}
		chatID = id
		if err := insertSharePermission(ctx, tx, chatID, sp); err != nil {
			return err
		}
		if err := upsertUserHistory(ctx, q, userID, chatID); err != nil {
			return err
		}
		if err := upsertItemLastAccessed(ctx, q, chatID); err != nil {
			return err
		}
		if err := insertOwnerEntityAccess(ctx, tx, chatID, userID); err != nil {
			return err
		}
		if err := q.CopyMessages(ctx, macrodb.CopyMessagesParams{ChatId: chatID, ChatId_2: sourceChatID}); err != nil {
			return fmt.Errorf("copy messages: %w", err)
		}
		return registerEntity(ctx, tx, chatID, userID)
	})
	if err != nil {
		return "", wrapErr(errInternal, "copy chat", err)
	}

	s.events.publish(ctx, chatID, EventChatCopied, chatCopiedMeta{
		ChatID:       chatID,
		SourceChatID: sourceChatID,
		Owner:        userID,
		Name:         name,
	})
	return chatID, nil
}

// patchChat mirrors ChatServiceImpl::patch: name/projectId/share-permission
// updates + project membership fixups; publishes chat.updated.
func (s *Service) patchChat(ctx context.Context, userID, chatID string, req PatchChatRequest) error {
	if req.Name != nil && utf8.RuneCountInString(*req.Name) > maxChatNameLen {
		return errf(errBadRequest, "name too long")
	}

	oldProject, err := s.chatProjectID(ctx, chatID)
	if err != nil {
		return err
	}
	var newProject *string
	if req.ProjectID != nil {
		newProject = req.ProjectID
	}
	projectChanging := req.ProjectID != nil && derefOr(req.ProjectID, "") != derefOr(oldProject, "")

	err = s.repo.withTx(ctx, func(tx pgx.Tx, q *macrodb.Queries) error {
		if err := touchChat(ctx, tx, chatID); err != nil {
			return err
		}
		if req.Name != nil {
			if err := q.PatchChatTransaction(ctx, macrodb.PatchChatTransactionParams{Name: *req.Name, ID: chatID}); err != nil {
				return err
			}
		}
		if req.ProjectID != nil {
			if *req.ProjectID == "" {
				if err := q.PatchChatTransaction3(ctx, chatID); err != nil {
					return err
				}
			} else {
				if err := q.PatchChatTransaction4(ctx, macrodb.PatchChatTransaction4Params{
					ProjectId: pgtype.Text{String: *req.ProjectID, Valid: true},
					ID:        chatID,
				}); err != nil {
					return err
				}
			}
		}
		if req.SharePermission != nil {
			if err := updateSharePermission(ctx, tx, chatID, req.SharePermission); err != nil {
				return err
			}
		}
		// TODO(team-share): port apply_team_share (team_share_access_level
		// grants on entity_access + SharePermission.team_share_* columns).
		// teamShareAccessLevel is parsed but not persisted yet.
		return nil
	})
	if err != nil {
		return wrapErr(errInternal, "patch chat", err)
	}

	// Project membership fixups (best-effort, matching Rust's inspect_err
	// logging without failing the request).
	if projectChanging {
		if oldProject != nil && *oldProject != "" && isUUID(chatID) && isUUID(*oldProject) {
			if err := s.repo.removeEntityFromProject(ctx, chatID, *oldProject); err != nil {
				slog.Warn("dcs: remove entity from project", "err", err)
			}
			if err := s.repo.updateProjectModified(ctx, *oldProject); err != nil {
				slog.Warn("dcs: update project modified", "err", err)
			}
		}
	}
	if newProject != nil && *newProject != "" && isUUID(chatID) && isUUID(*newProject) {
		if err := s.repo.addEntityToProject(ctx, chatID, *newProject); err != nil {
			slog.Warn("dcs: add entity to project", "err", err)
		}
		if err := s.repo.updateProjectModified(ctx, *newProject); err != nil {
			slog.Warn("dcs: update project modified", "err", err)
		}
	}

	s.events.publish(ctx, chatID, EventChatUpdated, chatUpdatedMeta{
		ChatID:                chatID,
		ActorUserID:           userID,
		Name:                  req.Name,
		PreviousProjectID:     oldProject,
		ProjectID:             newProject,
		SharePermissionUpdate: req.SharePermission != nil,
	})
	return nil
}

func derefOr(s *string, d string) string {
	if s == nil {
		return d
	}
	return *s
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// callerUserID extracts the acting user id or writes a 401 (mirrors the
// MacroAuthorizationExtractor<_, ActingUser/UserOrInternal> rejection).
func callerUserID(w http.ResponseWriter, r *http.Request) (auth.Caller, string, bool) {
	caller, ok := auth.FromContext(r.Context())
	if !ok || caller.UserID == "" {
		writeTextErr(w, http.StatusUnauthorized, "unauthorized")
		return caller, "", false
	}
	return caller, caller.UserID, true
}

// createChatHandler handles POST /chats.
func (s *Service) createChatHandler(w http.ResponseWriter, r *http.Request) {
	_, userID, ok := callerUserID(w, r)
	if !ok {
		return
	}
	var req CreateChatRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	name := defaultChatName
	if req.Name != nil && *req.Name != "" {
		name = *req.Name
	}
	id, err := s.createChat(r.Context(), userID, name, req.ProjectID)
	if err != nil {
		writeChatErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, StringIDResponse{ID: id})
}

// listChats handles GET /chats — the legacy DCS listing (model::chat::Chat[]).
func (s *Service) listChats(w http.ResponseWriter, r *http.Request) {
	_, userID, ok := callerUserID(w, r)
	if !ok {
		return
	}
	rows, err := s.q.GetChats(r.Context(), userID)
	if err != nil {
		writeChatErr(w, wrapErr(errInternal, "list chats", err))
		return
	}
	chats := make([]Chat, 0, len(rows))
	for _, row := range rows {
		chats = append(chats, Chat{
			ID:           row.ID,
			Name:         row.Name,
			UserID:       row.UserID,
			Model:        optionalString(row.Model),
			ProjectID:    textPtr(row.ProjectID),
			CreatedAt:    ts(row.CreatedAt),
			UpdatedAt:    ts(row.UpdatedAt),
			DeletedAt:    ts(row.DeletedAt),
			TokenCount:   int8Ptr(row.TokenCount),
			IsPersistent: row.IsPersistent,
		})
	}
	writeJSON(w, http.StatusOK, chats)
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func int8Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// getChat handles GET /chats/{chat_id} — view access (anonymous OK on public
// link share).
func (s *Service) getChat(w http.ResponseWriter, r *http.Request) {
	chatID := chi.URLParam(r, "chat_id")
	caller, authed := auth.FromContext(r.Context())
	info, err := s.requireAccess(r.Context(), caller, authed, chatID, AccessLevelView)
	if err != nil {
		writeAccessErr(w, err)
		return
	}
	resp, err := s.getChatResponse(r.Context(), info, chatID)
	if err != nil {
		writeChatErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// deleteChat handles DELETE /chats/{chat_id} — owner only, soft delete.
func (s *Service) deleteChat(w http.ResponseWriter, r *http.Request) {
	s.mutateChat(w, r, AccessLevelOwner, func(ctx context.Context, caller auth.Caller, authed bool, chatID string) error {
		return s.softDelete(ctx, chatID, eventActor(caller, authed))
	})
}

// permanentlyDeleteChat handles DELETE /chats/{chat_id}/permanent.
func (s *Service) permanentlyDeleteChat(w http.ResponseWriter, r *http.Request) {
	s.mutateChat(w, r, AccessLevelOwner, func(ctx context.Context, caller auth.Caller, authed bool, chatID string) error {
		return s.permanentlyDelete(ctx, chatID, eventActor(caller, authed))
	})
}

// revertDeleteChat handles PUT /chats/{chat_id}/revert_delete — owner only.
func (s *Service) revertDeleteChat(w http.ResponseWriter, r *http.Request) {
	s.mutateChat(w, r, AccessLevelOwner, func(ctx context.Context, caller auth.Caller, authed bool, chatID string) error {
		return s.restoreChat(ctx, chatID, eventActor(caller, authed))
	})
}

// copyChatHandler handles POST /chats/{chat_id}/copy — view access suffices.
func (s *Service) copyChatHandler(w http.ResponseWriter, r *http.Request) {
	chatID := chi.URLParam(r, "chat_id")
	caller, authed := auth.FromContext(r.Context())
	if !authed || caller.UserID == "" {
		writeTextErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, err := s.requireAccess(r.Context(), caller, authed, chatID, AccessLevelView); err != nil {
		writeAccessErr(w, err)
		return
	}
	id, err := s.copyChat(r.Context(), caller.UserID, chatID)
	if err != nil {
		writeChatErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, StringIDResponse{ID: id})
}

// patchChatHandler handles PATCH /chats/{chat_id} — owner only.
func (s *Service) patchChatHandler(w http.ResponseWriter, r *http.Request) {
	var req PatchChatRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	s.mutateChat(w, r, AccessLevelOwner, func(ctx context.Context, caller auth.Caller, authed bool, chatID string) error {
		return s.patchChat(ctx, caller.UserID, chatID, req)
	})
}

// getChatPermissions handles GET /chats/{chat_id}/permissions — edit access.
func (s *Service) getChatPermissions(w http.ResponseWriter, r *http.Request) {
	chatID := chi.URLParam(r, "chat_id")
	caller, authed := auth.FromContext(r.Context())
	if _, err := s.requireAccess(r.Context(), caller, authed, chatID, AccessLevelEdit); err != nil {
		writeAccessErr(w, err)
		return
	}
	perm, err := s.q.GetChatSharePermission(r.Context(), chatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeChatErr(w, errf(errNotFound, "chat not found"))
			return
		}
		writeChatErr(w, wrapErr(errInternal, "get permissions", err))
		return
	}
	var channels []ChannelSharePermission
	if len(perm.ChannelSharePermissions) > 0 {
		_ = json.Unmarshal(perm.ChannelSharePermissions, &channels)
	}
	var linkShare *string
	if perm.LinkShare.Valid {
		linkShare = &perm.LinkShare.String
	}
	var linkLevel *string
	if perm.LinkShareAccessLevel.Valid {
		lv := string(perm.LinkShareAccessLevel.AccessLevel)
		linkLevel = &lv
	}
	var teamLevel *string
	if perm.TeamShareAccessLevel.Valid {
		lv := string(perm.TeamShareAccessLevel.AccessLevel)
		teamLevel = &lv
	}
	writeJSON(w, http.StatusOK, GetChatPermissionsResponse{
		Permissions: SharePermissionV2{
			ID:                      perm.ID,
			LinkShare:               linkShare,
			LinkShareAccessLevel:    linkLevel,
			TeamShareAccessLevel:    teamLevel,
			Owner:                   perm.Owner,
			ChannelSharePermissions: channels,
		},
	})
}

// mutateChat is the shared auth + dispatch wrapper for owner/editor chat
// mutations (mirrors require_authenticated_user + ChatAccessLevelExtractor).
func (s *Service) mutateChat(w http.ResponseWriter, r *http.Request, minLevel string, fn func(ctx context.Context, caller auth.Caller, authed bool, chatID string) error) {
	chatID := chi.URLParam(r, "chat_id")
	caller, authed := auth.FromContext(r.Context())
	if !authed {
		writeTextErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, err := s.requireAccess(r.Context(), caller, authed, chatID, minLevel); err != nil {
		writeAccessErr(w, err)
		return
	}
	if err := fn(r.Context(), caller, authed, chatID); err != nil {
		writeChatErr(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
