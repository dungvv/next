// Chat access checks. Mirrors entity_access::domain::service +
// macro_db_client::share_permission::access_level::chat::get_highest_access_level_for_chats:
// owner short-circuit, entity_access rows (user/team/channel sources), and
// SharePermission link share (PUBLIC grants anonymous view, TEAM requires the
// caller to share a team with the owner).
package dcs

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// accessError mirrors the access-extractor / ensure_chat_exists failures in
// the Rust stack: (status, Json(ErrorResponse{message})) — note the 401
// (not 403) for insufficient access and the {"message": ...} body shape.
type accessError struct {
	status int
	msg    string
}

func (e *accessError) Error() string { return e.msg }

func accessErrf(status int, format string, args ...any) *accessError {
	return &accessError{status: status, msg: fmt.Sprintf(format, args...)}
}

// writeAccessErr writes the {"message": ...} body the Rust extractors emit.
// Non-access errors fall back to the chat error writer.
func writeAccessErr(w http.ResponseWriter, err error) {
	var ae *accessError
	if errors.As(err, &ae) {
		writeJSON(w, ae.status, map[string]string{"message": ae.msg})
		return
	}
	writeChatErr(w, err)
}

// chatAccessInfo carries the outcome of an access check.
type chatAccessInfo struct {
	// level is the caller's effective access level: view/comment/edit/owner.
	level string
	// internal is true for callers authenticated with the internal key; they
	// are granted owner-equivalent access.
	internal bool
	// userID is the acting macro user id ("" for anonymous public viewers).
	userID string
	// ownerID is the chat owner's user id.
	ownerID string
	// deleted is true when the chat is soft-deleted.
	deleted bool
}

// chatAccess resolves the caller's access level on a chat. The caller must be
// a real (or internal) user for anything above anonymous link-share access.
func (s *Service) chatAccess(ctx context.Context, caller auth.Caller, authed bool, chatID string) (*chatAccessInfo, error) {
	info := &chatAccessInfo{}

	owner, err := s.q.GetOwnerAndDeleted2(ctx, chatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ensure_chat_exists: 404 {"message": "chat with id \"x\" was not found"}
			return nil, accessErrf(http.StatusNotFound, "chat with id %q was not found", chatID)
		}
		return nil, wrapErr(errInternal, "get chat owner", err)
	}
	info.ownerID = owner.UserID
	info.deleted = owner.DeletedAt.Valid

	if authed && caller.Internal {
		info.internal = true
		info.userID = caller.UserID
		info.level = AccessLevelOwner
		return info, nil
	}

	if authed && caller.UserID != "" {
		info.userID = caller.UserID
		if caller.UserID == info.ownerID {
			info.level = AccessLevelOwner
			return info, nil
		}
		levels, err := s.q.GetHighestAccessLevelForChats(ctx, macrodb.GetHighestAccessLevelForChatsParams{
			Column1: []string{chatID},
			UserID:  caller.UserID,
		})
		if err != nil {
			return nil, wrapErr(errInternal, "get access level", err)
		}
		for _, row := range levels {
			if row.ChatID == chatID && accessRank(row.AccessLevel) > accessRank(info.level) {
				info.level = row.AccessLevel
			}
		}
		return info, nil
	}

	// Anonymous caller: only public link share grants access.
	perm, err := s.q.GetChatSharePermission(ctx, chatID)
	if err == nil && perm.LinkShare.Valid && perm.LinkShare.String == LinkSharePublic {
		level := AccessLevelView
		if perm.LinkShareAccessLevel.Valid && perm.LinkShareAccessLevel.AccessLevel != "" {
			level = string(perm.LinkShareAccessLevel.AccessLevel)
		}
		info.level = level
	}
	return info, nil
}

// requireAccess resolves access and fails when below min. Deleted chats are
// only accessible to the owner (mirroring ChatAccessLevelExtractor's
// "only owner can access deleted resource" check). Returns the access info;
// anonymous callers get level "".
func (s *Service) requireAccess(ctx context.Context, caller auth.Caller, authed bool, chatID, min string) (*chatAccessInfo, error) {
	info, err := s.chatAccess(ctx, caller, authed, chatID)
	if err != nil {
		return nil, err
	}
	if info.deleted && accessRank(info.level) < accessRank(AccessLevelOwner) {
		return nil, accessErrf(http.StatusUnauthorized, "only owner can access deleted resource")
	}
	if accessRank(info.level) < accessRank(min) {
		return nil, accessErrf(http.StatusUnauthorized, "User does not have access to the requested resource")
	}
	return info, nil
}

// modelAccess mirrors ChatModelAccess: free users get FREE_MODEL only,
// professional users get every model. Permission IDs come from the
// roles/permissions tables via GetUserPermissions2.
const (
	freeModel        = "anthropic/claude-haiku-4-5"
	paidDefaultModel = "anthropic/claude-sonnet-5"
	permProfessional = "ReadProfessionalFeatures"
)

// modelAccessFor resolves the caller's model entitlement. Internal callers
// get professional access; a missing/again broken permissions lookup falls
// back to free access (fail-closed, matching the Rust 402 semantics).
func (s *Service) modelAccessFor(ctx context.Context, caller auth.Caller) (professional bool, err error) {
	if caller.Internal {
		return true, nil
	}
	perms, err := s.q.GetUserPermissions2(ctx, caller.UserID)
	if err != nil {
		return false, wrapErr(errInternal, "get user permissions", err)
	}
	for _, p := range perms {
		if p == permProfessional {
			return true, nil
		}
	}
	return false, nil
}

// hasModelAccess mirrors ModelAccessServiceImpl::has_access.
func hasModelAccess(professional bool, modelID string) bool {
	return professional || modelID == freeModel
}

// bestModel mirrors ModelAccessServiceImpl::best_model.
func bestModel(professional bool) string {
	if professional {
		return paidDefaultModel
	}
	return freeModel
}
