// Chat history endpoints — Go port of
// services/document_cognition_service/src/api/chats/chat_history{,_batch_messages}.rs.
// Both return model::chat::ChatHistory: conversations grouped by chat, each
// message deduplicated on (content, createdAt) with aggregated attachment ids.
package dcs

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// buildChatHistory mirrors macro_db_client::chat_history::build_chat_history:
// rows → grouped conversations; a row without attachments contributes a
// message with an empty attachment list.
func buildChatHistory(rows []chatHistoryRow) ChatHistory {
	type msgKey struct {
		content string
		at      int64 // UnixNano — map keys must be comparable
	}
	type conv struct {
		title    string
		order    []msgKey
		messages map[msgKey]*MessageWithAttachments
	}
	chats := map[string]*conv{}
	var order []string

	for _, row := range rows {
		c, ok := chats[row.chatID]
		if !ok {
			c = &conv{title: row.chatTitle, messages: map[msgKey]*MessageWithAttachments{}}
			chats[row.chatID] = c
			order = append(order, row.chatID)
		}
		key := msgKey{content: row.content, at: row.createdAt.UnixNano()}
		m, ok := c.messages[key]
		if !ok {
			m = &MessageWithAttachments{
				Content:       row.content,
				Date:          row.createdAt,
				AttachmentIDs: []string{},
			}
			c.messages[key] = m
			c.order = append(c.order, key)
		}
		if row.attachmentID != nil {
			m.AttachmentIDs = append(m.AttachmentIDs, *row.attachmentID)
		}
	}

	out := ChatHistory{Conversation: []ConversationRecord{}}
	for _, chatID := range order {
		c := chats[chatID]
		rec := ConversationRecord{
			ChatID:   chatID,
			Title:    c.title,
			Messages: make([]MessageWithAttachments, 0, len(c.order)),
		}
		for _, key := range c.order {
			rec.Messages = append(rec.Messages, *c.messages[key])
		}
		out.Conversation = append(out.Conversation, rec)
	}
	return out
}

// getChatHistory handles GET /chats/history/{chat_id} — view access required
// (ChatAccessLevelExtractor<ViewAccessLevel> in Rust).
func (s *Service) getChatHistory(w http.ResponseWriter, r *http.Request) {
	chatID := chi.URLParam(r, "chat_id")
	caller, authed := auth.FromContext(r.Context())
	if _, err := s.requireAccess(r.Context(), caller, authed, chatID, AccessLevelView); err != nil {
		writeAccessErr(w, err)
		return
	}
	rows, err := s.repo.chatHistory(r.Context(), chatID)
	if err != nil {
		writeDcsChatErr(w, wrapErr(errInternal, "failed to retrieve chat history", err))
		return
	}
	writeJSON(w, http.StatusOK, buildChatHistory(rows))
}

// getChatHistoryBatchMessages handles POST /chats/history_batch_messages —
// authenticated callers only; view access on every owning chat is required.
func (s *Service) getChatHistoryBatchMessages(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok || caller.UserID == "" {
		writeTextErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req ChatHistoryBatchMessagesRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if len(req.MessageIDs) == 0 {
		writeObjErr(w, http.StatusNotFound, "No message IDs provided")
		return
	}

	chatIDs, err := s.q.GetChatIdsForMessages(r.Context(), req.MessageIDs)
	if err != nil {
		writeDcsChatErr(w, wrapErr(errInternal, "messages not found", err))
		return
	}

	// Internal callers skip the per-chat access check (UserOrInternal).
	if !caller.Internal {
		levels, err := s.q.GetHighestAccessLevelForChats(r.Context(), macrodb.GetHighestAccessLevelForChatsParams{
			Column1: chatIDs,
			UserID:  caller.UserID,
		})
		if err != nil {
			writeDcsChatErr(w, wrapErr(errInternal, "failed to check chat access", err))
			return
		}
		granted := map[string]string{}
		for _, row := range levels {
			granted[row.ChatID] = row.AccessLevel
		}
		for _, chatID := range chatIDs {
			if _, ok := granted[chatID]; !ok {
				writeObjErr(w, http.StatusForbidden, "Access denied to chat")
				return
			}
		}
	}

	rows, err := s.repo.chatHistoryForMessages(r.Context(), req.MessageIDs)
	if err != nil {
		writeDcsChatErr(w, wrapErr(errInternal, "failed to retrieve chat history", err))
		return
	}
	writeJSON(w, http.StatusOK, buildChatHistory(rows))
}
