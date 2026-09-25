// Message handlers for /channels/{id}/message* — ports the persistence +
// side-effect pipeline of crates/channels (message_delivery + side_effects).
package chat

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/pkg/store/commsdb"
)

// postMessage — POST /channels/{channel_id}/message and
// POST /channels/{channel_id}/messages.
func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	c, ok := s.requireParticipant(w, r, channelID)
	if !ok {
		return
	}
	var req PostMessageRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Content == "" && len(req.Attachments) == 0 {
		writeError(w, http.StatusBadRequest, "content or attachments required")
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id generation failed")
		return
	}
	m := &messageRow{ID: id, ChannelID: channelID, ThreadID: req.ThreadID, SenderID: c.UserID, Content: req.Content}
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(r.Context())
	if err := s.db.createMessage(r.Context(), tx, m); err != nil {
		writeError(w, http.StatusInternalServerError, "message insert failed")
		return
	}
	atts, err := s.db.addAttachments(r.Context(), tx, channelID, id, req.Attachments)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "attachment insert failed")
		return
	}
	// Persist mentions; returns mentioned users that are active participants.
	mentionedUsers, err := s.createMentionsTx(r.Context(), tx, id, req.Mentions)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mention insert failed")
		return
	}
	_ = s.db.touchChannel(r.Context(), tx, channelID)
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	// Side effects — errors are logged, never fatal (Rust: side effects must
	// not suppress events for a committed message).
	s.afterMessagePosted(r.Context(), channelID, m, atts, req.Mentions, mentionedUsers, req.Nonce)
	writeJSON(w, http.StatusOK, PostMessageResponse{ID: id.String(), Nonce: req.Nonce})
}

// createMentionsTx persists entity mentions (source 'message') and returns
// user ids that are active channel participants — ports
// comms_db_client::create_message_mentions.
func (s *Server) createMentionsTx(ctx context.Context, tx pgx.Tx, messageID uuid.UUID, mentions []SimpleMention) ([]string, error) {
	if len(mentions) == 0 {
		return nil, nil
	}
	ets, ids := make([]string, 0, len(mentions)), make([]string, 0, len(mentions))
	for _, m := range mentions {
		if m.EntityType == "" || m.EntityID == "" {
			continue
		}
		ets = append(ets, m.EntityType)
		ids = append(ids, m.EntityID)
	}
	if len(ets) == 0 {
		return nil, nil
	}
	return s.db.q.WithTx(tx).CreateMessageMentions(ctx, commsdb.CreateMessageMentionsParams{
		MessageID:   pgUUID(messageID),
		EntityTypes: ets,
		EntityIds:   ids,
	})
}

// afterMessagePosted ports handle_message_posted: realtime frames, then
// notification effects in the Rust order (mentions → reply/first-invite/send).
func (s *Server) afterMessagePosted(ctx context.Context, channelID uuid.UUID, m *messageRow, atts []MessageAttachment, mentions []SimpleMention, mentionedUsers []string, nonce *string) {
	participants, err := s.db.participantUserIDs(ctx, channelID)
	if err != nil || len(participants) == 0 {
		return
	}
	s.fx.commsMessage(participants, &MutatedMessage{
		ID: m.ID, ChannelID: channelID, ThreadID: m.ThreadID, SenderID: m.SenderID,
		Content: m.Content, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		EditedAt: m.EditedAt, DeletedAt: m.DeletedAt,
	}, nonce)
	// Bot trigger candidates dispatch for every user-authored post (the
	// agent.trigger consumer decides whether a bot is invoked).
	s.fx.dispatchBotTrigger(ctx, channelID, &MutatedMessage{
		ID: m.ID, ChannelID: channelID, ThreadID: m.ThreadID, SenderID: m.SenderID,
		Content: m.Content, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		EditedAt: m.EditedAt, DeletedAt: m.DeletedAt,
	}, mentions)
	if len(atts) > 0 {
		s.fx.commsAttachment(participants, channelID, m.ID, atts, nonce)
	}
	s.db.upsertActivityBestEffort(ctx, m.SenderID, channelID, "interact")

	ch, err := s.db.getChannel(ctx, channelID)
	if err != nil {
		return
	}
	excluded := map[string]bool{m.SenderID: true}

	docMentions := []string{}
	for _, mt := range mentions {
		switch mt.EntityType {
		case "user":
			excluded[mt.EntityID] = true
		case "document":
			docMentions = append(docMentions, mt.EntityID)
		}
	}

	// User mentions → channel_mention (only mentions that are active channel
	// participants get notified — mentionedUsers from CreateMessageMentions).
	if len(mentionedUsers) > 0 {
		recipients := exclude(mentionedUsers, map[string]bool{m.SenderID: true})
		threadRef := m.ThreadID
		if threadRef == nil {
			threadRef = &m.ID
		}
		s.fx.sendIngress(ctx, ch, &m.SenderID, recipients,
			&notificationEntity{EntityType: "channel_message", EntityID: threadRef.String()},
			"channel_mention", map[string]any{
				"messageId":         m.ID.String(),
				"messageContent":    m.Content,
				"threadId":          optUUID(m.ThreadID),
				"senderDisplayName": displayName(m.SenderID),
			}, "New mention", truncate(m.Content, 120))
	}

	// Document mentions → document_mention to participants minus sender.
	for _, docID := range docMentions {
		doc := s.documentBrief(ctx, docID)
		if doc == nil {
			continue
		}
		s.fx.sendIngress(ctx, ch, &m.SenderID, exclude(participants, map[string]bool{m.SenderID: true}),
			nil, "document_mention", map[string]any{
				"documentName":      doc.name,
				"owner":             map[string]any{"type": "user", "id": doc.owner},
				"fileType":          doc.fileType,
				"messageId":         m.ID.String(),
				"messageContent":    m.Content,
				"threadId":          optUUID(m.ThreadID),
				"senderDisplayName": displayName(m.SenderID),
			}, "Document shared", doc.name)
	}

	if m.ThreadID != nil {
		threadPeeps, err := s.db.threadParticipantUserIDs(ctx, *m.ThreadID)
		if err != nil || len(threadPeeps) == 0 {
			return
		}
		s.fx.sendIngress(ctx, ch, &m.SenderID, exclude(threadPeeps, excluded),
			&notificationEntity{EntityType: "channel_message", EntityID: m.ThreadID.String()},
			"channel_message_reply", map[string]any{
				"threadId":             m.ThreadID.String(),
				"messageId":            m.ID.String(),
				"userId":               m.SenderID,
				"senderDisplayName":    displayName(m.SenderID),
				"messageContent":       m.Content,
				"threadParentSenderId": s.threadParentSender(ctx, *m.ThreadID),
			}, "New reply", truncate(m.Content, 120))
		return
	}

	if s.isFirstTopLevelMessage(ctx, channelID) {
		// First user message doubles as the invite (Rust
		// send_first_message_invites: registered → push+realtime,
		// unregistered → invite email).
		s.fx.sendIngress(ctx, ch, &m.SenderID, exclude(participants, excluded),
			nil, "channel_invite", map[string]any{
				"invitedBy":      m.SenderID,
				"channelName":    chName(ch),
				"messageContent": m.Content,
			}, "Channel invite", truncate(m.Content, 120))
		return
	}
	s.fx.sendIngress(ctx, ch, &m.SenderID, exclude(participants, excluded),
		nil, "channel_message_send", map[string]any{
			"sender":            m.SenderID,
			"senderDisplayName": displayName(m.SenderID),
			"messageContent":    m.Content,
			"messageId":         m.ID.String(),
		}, chName(ch), truncate(m.Content, 120))
}

type docBrief struct {
	name     string
	owner    string
	fileType *string
}

// documentBrief looks up Document name/owner for document_mention metadata.
func (s *Server) documentBrief(ctx context.Context, docID string) *docBrief {
	var b docBrief
	err := s.db.pool.QueryRow(ctx,
		`SELECT name, owner, file_type FROM "Document" WHERE id = $1`, docID).
		Scan(&b.name, &b.owner, &b.fileType)
	if err != nil {
		return nil
	}
	return &b
}

func (s *Server) threadParentSender(ctx context.Context, threadID uuid.UUID) *string {
	var sender *string
	_ = s.db.pool.QueryRow(ctx,
		`SELECT sender_id FROM comms_messages WHERE id = $1`, threadID).Scan(&sender)
	return sender
}

func (s *Server) isFirstTopLevelMessage(ctx context.Context, channelID uuid.UUID) bool {
	var n int64
	_ = s.db.pool.QueryRow(ctx,
		`SELECT count(*) FROM comms_messages WHERE channel_id = $1 AND thread_id IS NULL`, channelID).Scan(&n)
	return n <= 1
}

// patchMessage — PATCH /channels/{channel_id}/message/{message_id}.
func (s *Server) patchMessage(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	messageID, ok := parseUUIDParam(w, r, "message_id")
	if !ok {
		return
	}
	c, ok := s.requireParticipant(w, r, channelID)
	if !ok {
		return
	}
	var req PatchMessageRequest
	if !decodeBody(w, r, &req) {
		return
	}
	existing, err := s.db.getMessage(r.Context(), messageID)
	if errors.Is(err, errNotFound) || existing.ChannelID != channelID {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	// Only the sender (or an internal caller) may edit.
	if existing.SenderID != c.UserID && !(c.Internal && c.UserID == auth.InternalUserID) {
		writeError(w, http.StatusForbidden, "only the sender can edit")
		return
	}
	m, err := s.db.patchMessage(r.Context(), messageID, req.Content)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if req.Mentions != nil {
		// Replace mentions (Rust delete+insert semantics).
		if _, err := s.db.pool.Exec(r.Context(), `
			DELETE FROM comms_entity_mentions
			WHERE source_entity_type = 'message' AND source_entity_id = $1`, messageID.String()); err == nil {
			_, _ = s.createMentions(r.Context(), messageID, *req.Mentions)
		}
	}
	if req.AttachmentIDsToDelete != nil && len(*req.AttachmentIDsToDelete) > 0 {
		_ = s.db.deleteAttachments(r.Context(), messageID, *req.AttachmentIDsToDelete)
	}
	var newAtts []MessageAttachment
	if req.AttachmentsToAdd != nil && len(*req.AttachmentsToAdd) > 0 {
		newAtts, _ = s.db.addAttachments(r.Context(), nil, channelID, messageID, *req.AttachmentsToAdd)
	}
	_ = s.db.touchChannel(r.Context(), nil, channelID)

	if uids, err := s.db.participantUserIDs(r.Context(), channelID); err == nil {
		s.fx.commsMessage(uids, &MutatedMessage{
			ID: m.ID, ChannelID: m.ChannelID, ThreadID: m.ThreadID, SenderID: m.SenderID,
			TriggeredBy: m.TriggeredBy, Content: m.Content, CreatedAt: m.CreatedAt,
			UpdatedAt: m.UpdatedAt, EditedAt: m.EditedAt, DeletedAt: m.DeletedAt,
		}, req.Nonce)
		if len(newAtts) > 0 || (req.AttachmentIDsToDelete != nil && len(*req.AttachmentIDsToDelete) > 0) {
			cur, _ := s.db.messageAttachments(r.Context(), []uuid.UUID{messageID})
			s.fx.commsAttachment(uids, channelID, messageID, cur[messageID], req.Nonce)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": messageID.String()})
}

// createMentions is the non-tx variant for mention replacement.
func (s *Server) createMentions(ctx context.Context, messageID uuid.UUID, mentions []SimpleMention) ([]string, error) {
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	users, err := s.createMentionsTx(ctx, tx, messageID, mentions)
	if err != nil {
		return nil, err
	}
	return users, tx.Commit(ctx)
}

// deleteMessage — DELETE /channels/{channel_id}/message/{message_id}?nonce=.
func (s *Server) deleteMessage(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	messageID, ok := parseUUIDParam(w, r, "message_id")
	if !ok {
		return
	}
	c, ok := s.requireParticipant(w, r, channelID)
	if !ok {
		return
	}
	existing, err := s.db.getMessage(r.Context(), messageID)
	if errors.Is(err, errNotFound) || existing.ChannelID != channelID {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if existing.SenderID != c.UserID {
		// Channel owner/admin may delete others' messages.
		_, role, _ := s.db.isParticipant(r.Context(), channelID, c.UserID)
		if role != RoleOwner && role != RoleAdmin && !(c.Internal && c.UserID == auth.InternalUserID) {
			writeError(w, http.StatusForbidden, "cannot delete this message")
			return
		}
	}
	m, err := s.db.softDeleteMessage(r.Context(), messageID)
	if errors.Is(err, errNotFound) {
		w.WriteHeader(http.StatusOK) // already deleted — idempotent
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	nonce := r.URL.Query().Get("nonce")
	var nonceP *string
	if nonce != "" {
		nonceP = &nonce
	}
	if uids, err := s.db.participantUserIDs(r.Context(), channelID); err == nil {
		s.fx.commsMessage(uids, &MutatedMessage{
			ID: m.ID, ChannelID: m.ChannelID, ThreadID: m.ThreadID, SenderID: m.SenderID,
			TriggeredBy: m.TriggeredBy, Content: m.Content, CreatedAt: m.CreatedAt,
			UpdatedAt: m.UpdatedAt, EditedAt: m.EditedAt, DeletedAt: m.DeletedAt,
		}, nonceP)
	}
	w.WriteHeader(http.StatusOK)
}

// postReaction — POST /channels/{channel_id}/reaction.
func (s *Server) postReaction(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	c, ok := s.requireParticipant(w, r, channelID)
	if !ok {
		return
	}
	var req PostReactionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	messageID, err := uuid.Parse(req.MessageID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message_id")
		return
	}
	if msg, err := s.db.getMessage(r.Context(), messageID); err != nil || msg.ChannelID != channelID {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	switch req.Action {
	case "add":
		err = s.db.addReaction(r.Context(), messageID, req.Emoji, c.UserID)
	case "remove":
		err = s.db.removeReaction(r.Context(), messageID, req.Emoji, c.UserID)
	default:
		writeError(w, http.StatusBadRequest, "action must be add|remove")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reaction failed")
		return
	}
	s.db.upsertActivityBestEffort(r.Context(), c.UserID, channelID, "interact")
	counts, _ := s.db.countedReactions(r.Context(), []uuid.UUID{messageID})
	if uids, err := s.db.participantUserIDs(r.Context(), channelID); err == nil {
		s.fx.commsReaction(uids, channelID, messageID, counts[messageID], req.Nonce)
	}
	w.WriteHeader(http.StatusOK)
}

// postTyping — POST /channels/{channel_id}/typing.
func (s *Server) postTyping(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	c, ok := s.requireParticipant(w, r, channelID)
	if !ok {
		return
	}
	var req PostTypingRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Action != "start" && req.Action != "stop" {
		writeError(w, http.StatusBadRequest, "action must be start|stop")
		return
	}
	uids, _ := s.db.participantUserIDs(r.Context(), channelID)
	s.fx.commsTyping(uids, channelID, c.UserID, req.Action, req.ThreadID, req.Nonce)
	w.WriteHeader(http.StatusOK)
}

// ---- queries ----

// getChannelMessages — GET /channels/{channel_id}/messages.
// Query params: limit, before (RFC3339 cursor), after.
func (s *Server) getChannelMessages(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	if _, ok := s.requireParticipant(w, r, channelID); !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	var before, after *time.Time
	if v := q.Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			before = &t
		}
	}
	if v := q.Get("after"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			after = &t
		}
	}
	rows, err := s.db.listMessages(r.Context(), channelID, before, after, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	msgs, err := s.hydrate(r.Context(), rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// getMessagesCatchUp — GET /channels/{channel_id}/messages/catch-up?after=.
func (s *Server) getMessagesCatchUp(w http.ResponseWriter, r *http.Request) {
	s.getChannelMessages(w, r)
}

// getThreadReplies — GET /channels/{channel_id}/messages/{message_id}/replies.
func (s *Server) getThreadReplies(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	rootID, ok := parseUUIDParam(w, r, "message_id")
	if !ok {
		return
	}
	if _, ok := s.requireParticipant(w, r, channelID); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.db.threadReplies(r.Context(), rootID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	msgs, err := s.hydrate(r.Context(), rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// getMessageContext — GET /channels/{channel_id}/messages/{message_id}/context.
// Returns N messages around the target (older + newer + the message itself).
func (s *Server) getMessageContext(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	messageID, ok := parseUUIDParam(w, r, "message_id")
	if !ok {
		return
	}
	if _, ok := s.requireParticipant(w, r, channelID); !ok {
		return
	}
	target, err := s.db.getMessage(r.Context(), messageID)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	const window = 25
	older, err := s.db.listMessages(r.Context(), channelID, &target.CreatedAt, nil, window+1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	newer, err := s.db.listMessages(r.Context(), channelID, nil, &target.CreatedAt, window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	rows := append(append([]messageRow{}, older...), newer...)
	msgs, err := s.hydrate(r.Context(), rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// resolveMessage — GET /channels/{channel_id}/messages/{message_id}/resolve.
func (s *Server) resolveMessage(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	messageID, ok := parseUUIDParam(w, r, "message_id")
	if !ok {
		return
	}
	if _, ok := s.requireParticipant(w, r, channelID); !ok {
		return
	}
	m, err := s.db.getMessage(r.Context(), messageID)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	kind := "top_level_message"
	threadID := m.ID
	if m.ThreadID != nil {
		kind = "thread_reply"
		threadID = *m.ThreadID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message_id": m.ID,
		"channel_id": m.ChannelID,
		"kind":       kind,
		"thread_id":  threadID,
		"created_at": m.CreatedAt,
	})
}

// hydrate enriches message rows with thread info, reactions, attachments.
func (s *Server) hydrate(ctx context.Context, rows []messageRow) ([]ChannelMessage, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	reactions, err := s.db.countedReactions(ctx, ids)
	if err != nil {
		return nil, err
	}
	atts, err := s.db.messageAttachments(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelMessage, 0, len(rows))
	for _, m := range rows {
		msg := ChannelMessage{
			ID: m.ID, ChannelID: m.ChannelID, SenderID: m.SenderID,
			TriggeredBy: m.TriggeredBy, Content: m.Content,
			CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
			EditedAt: m.EditedAt, DeletedAt: m.DeletedAt,
			Reactions:   nonNilReactions(reactions[m.ID]),
			Attachments: nonNilAttachments(atts[m.ID]),
			Thread:      ThreadInfo{Preview: []ThreadReply{}},
		}
		if m.ThreadID == nil {
			count, latest, err := s.db.threadInfo(ctx, m.ID)
			if err == nil && count > 0 {
				msg.Thread.ReplyCount = count
				msg.Thread.LatestReplyAt = latest
				if preview, err := s.db.threadReplies(ctx, m.ID, 3); err == nil {
					for _, p := range preview {
						msg.Thread.Preview = append(msg.Thread.Preview, ThreadReply{
							ID: p.ID, SenderID: p.SenderID, TriggeredBy: p.TriggeredBy,
							Content: p.Content, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
							EditedAt:    p.EditedAt,
							Reactions:   nonNilReactions(reactions[p.ID]),
							Attachments: nonNilAttachments(atts[p.ID]),
						})
					}
				}
			}
		}
		out = append(out, msg)
	}
	return out, nil
}

func nonNilReactions(v []CountedReaction) []CountedReaction {
	if v == nil {
		return []CountedReaction{}
	}
	return v
}

func nonNilAttachments(v []MessageAttachment) []MessageAttachment {
	if v == nil {
		return []MessageAttachment{}
	}
	return v
}

// getChannelAttachments — GET /channels/{channel_id}/attachments.
func (s *Server) getChannelAttachments(w http.ResponseWriter, r *http.Request) {
	channelID, ok := parseUUIDParam(w, r, "channel_id")
	if !ok {
		return
	}
	if _, ok := s.requireParticipant(w, r, channelID); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	atts, err := s.db.channelAttachments(r.Context(), channelID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, atts)
}

// helpers

func exclude(ids []string, skip map[string]bool) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if !skip[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func chName(ch *channelRow) string {
	if ch == nil || ch.Name == nil {
		return ""
	}
	return *ch.Name
}

func optUUID(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
