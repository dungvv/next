// AI chat streaming — Go port of
// services/document_cognition_service/src/api/stream/chat_message.rs and
// stop.rs. The Rust service publishes stream items into a Redis-backed
// durable stream consumed by connection_gateway; here each item is buffered
// in-process (serving the SSE replay endpoint) and published on the NATS
// subject dcs.stream.<stream_id> for gateway-style consumers.
package dcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// streamItemSubject is the NATS subject prefix for stream items:
// "dcs.stream.<stream_id>". Mirrors the Redis stream key the Rust
// RedisPostgresStreamRepo writes to.
const streamItemSubject = "dcs.stream"

// streamNotifySubject mirrors the Redis `stream:notifications` channel —
// published once when a stream buffer is created and again when it closes, so
// gateway-style consumers can discover streams without polling.
const streamNotifySubject = "dcs.stream.notifications"

// streamBufferTTL is how long a finished stream's items stay replayable
// (mirrors the closed-stream TTL in the Redis repo).
const streamBufferTTL = 10 * time.Minute

// ---------------------------------------------------------------------------
// stream hub — in-process durable buffer + local cancellation registry
// ---------------------------------------------------------------------------

type streamBuf struct {
	chatID string // owning chat — the replay endpoint checks access on it
	items  [][]byte
	done   bool
	cancel context.CancelFunc
	subs   map[chan []byte]struct{}
}

// streamHub tracks in-flight streams: buffered items for replay, live
// subscribers for the SSE endpoint, and the cancel func for stop.
type streamHub struct {
	mu      sync.Mutex
	streams map[string]*streamBuf
}

func newStreamHub() *streamHub {
	return &streamHub{streams: map[string]*streamBuf{}}
}

// open registers a new stream buffer bound to its owning chat.
func (h *streamHub) open(streamID, chatID string, cancel context.CancelFunc) *streamBuf {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := &streamBuf{chatID: chatID, cancel: cancel, subs: map[chan []byte]struct{}{}}
	h.streams[streamID] = b
	return b
}

// chatOf returns the chat a stream belongs to.
func (h *streamHub) chatOf(streamID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.streams[streamID]
	if !ok {
		return "", false
	}
	return b.chatID, true
}

// publish appends an item and fans it out to subscribers.
func (h *streamHub) publish(streamID string, item []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.streams[streamID]
	if !ok || b.done {
		return
	}
	b.items = append(b.items, item)
	for ch := range b.subs {
		select {
		case ch <- item:
		default: // slow consumer — drop (buffered replay covers reconnects)
		}
	}
}

// finish marks the stream complete and closes subscriber channels. The
// buffer stays replayable until expire removes it.
func (h *streamHub) finish(streamID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.streams[streamID]
	if !ok || b.done {
		return
	}
	b.done = true
	for ch := range b.subs {
		close(ch)
	}
	b.subs = map[chan []byte]struct{}{}
}

// expire deletes the buffer entirely.
func (h *streamHub) expire(streamID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.streams, streamID)
}

// cancel invokes the registered cancel func, if the stream is still tracked.
func (h *streamHub) cancel(streamID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.streams[streamID]
	if !ok || b.done || b.cancel == nil {
		return false
	}
	b.cancel()
	return true
}

// subscribe returns the replay buffer and a channel for new items. done=true
// means the stream already finished — only the replay applies.
func (h *streamHub) subscribe(streamID string) (replay [][]byte, ch chan []byte, done bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.streams[streamID]
	if !ok {
		return nil, nil, false
	}
	replay = append([][]byte(nil), b.items...)
	if b.done {
		return replay, nil, true
	}
	ch = make(chan []byte, 64)
	b.subs[ch] = struct{}{}
	return replay, ch, false
}

// unsubscribe detaches a subscriber channel.
func (h *streamHub) unsubscribe(streamID string, ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b, ok := h.streams[streamID]; ok {
		delete(b.subs, ch)
	}
}

// publishNats fans a stream item out to NATS for gateway-style consumers.
// Fire-and-forget: the local buffer is the source of truth.
func (s *Service) publishNats(subject string, payload []byte) {
	if s.nc == nil {
		return
	}
	if err := s.nc.Publish(subject, payload); err != nil {
		slog.Warn("dcs: nats stream publish", "subject", subject, "err", err)
	}
}

func (s *Service) emitStreamItem(streamID string, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		slog.Error("dcs: marshal stream item", "stream", streamID, "err", err)
		return
	}
	s.hub.publish(streamID, payload)
	s.publishNats(streamItemSubject+"."+streamID, payload)
}

// ---------------------------------------------------------------------------
// POST /stream/chat/message
// ---------------------------------------------------------------------------

// attachmentEntityTypes mirrors the EntityType→AttachmentType→EntityType
// mapping in store_incoming_message + attachment_type_to_entity_type:
// document|static_file|channel|email_thread|project|skill are persisted;
// every other entity type is dropped.
func attachmentEntityType(t EntityType) (string, bool) {
	switch t {
	case EntityTypeDocument:
		return "document", true
	case EntityTypeStaticFile:
		return "static_file", true
	case EntityTypeChannel:
		return "channel", true
	case EntityTypeEmailThread:
		return "email_thread", true
	case EntityTypeProject:
		return "project", true
	case EntityTypeSkill:
		return "skill", true
	default:
		return "", false
	}
}

// streamChatError writes the ChatMessageError body ({error, stream_id}).
func streamChatError(w http.ResponseWriter, status int, streamID *string, msg string) {
	writeJSON(w, status, ChatMessageError{Error: msg, StreamID: streamID})
}

// sendChatMessage handles POST /stream/chat/message.
func (s *Service) sendChatMessage(w http.ResponseWriter, r *http.Request) {
	caller, userID, ok := callerUserID(w, r)
	if !ok {
		return
	}
	var req SendChatMessageRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	messageID := uuid.NewString()
	streamID := messageID // Rust uses the message id as the stream id

	// Model entitlement: free users only get the free model.
	pro, err := s.modelAccessFor(r.Context(), caller)
	if err != nil || !hasModelAccess(pro, req.Model) {
		streamChatError(w, http.StatusForbidden, &streamID,
			fmt.Sprintf("No access to model %s", req.Model))
		return
	}
	model := req.Model

	// Resolve the chat — provided id or a fresh one.
	var chatID string
	var prior []storedMessage
	createdNew := false
	if req.ChatID != nil && *req.ChatID != "" {
		chatID = *req.ChatID
		info, err := s.chatAccess(r.Context(), caller, true, chatID)
		if err != nil {
			var ae *accessError
			if errors.As(err, &ae) && ae.status == http.StatusNotFound {
				// Rust creates a new chat when the requested id is unknown.
				createdNew = true
			} else {
				streamChatError(w, http.StatusBadRequest, &streamID,
					fmt.Sprintf("Permission check failed: %v", err))
				return
			}
		} else if accessRank(info.level) < accessRank(AccessLevelEdit) {
			streamChatError(w, http.StatusBadRequest, &streamID,
				"Insufficient permissions to send messages")
			return
		}
	}
	if req.ChatID == nil || *req.ChatID == "" || createdNew {
		id, err := s.createStreamChat(r.Context(), userID, model)
		if err != nil {
			streamChatError(w, http.StatusBadRequest, &streamID, "Failed to create chat")
			return
		}
		chatID = id
		createdNew = true
	} else {
		msgs, err := s.q.GetMessages(r.Context(), chatID)
		if err != nil {
			streamChatError(w, http.StatusBadRequest, &streamID, "Failed to load chat")
			return
		}
		for _, m := range msgs {
			prior = append(prior, storedMessage{
				role:    m.Role,
				content: MessageContent{raw: json.RawMessage(m.Content)},
			})
		}
	}
	autoRename := createdNew || len(prior) == 0

	// Persist the incoming user message (attachments recorded for known
	// entity types only, mirroring store_incoming_message).
	var attachments []newAttachment
	for _, e := range req.Attachments {
		if et, ok := attachmentEntityType(e.EntityType); ok {
			attachments = append(attachments, newAttachment{entityType: et, entityID: e.EntityID})
		}
	}
	userMessageID, err := s.repo.createMessage(r.Context(), chatID, newMessage{
		content:     TextContent(req.Content),
		role:        RoleUser,
		model:       model,
		attachments: attachments,
		createdAt:   time.Now(),
	})
	if err != nil {
		slog.Error("dcs: store incoming message", "err", err, "chat", chatID)
		streamChatError(w, http.StatusBadRequest, &streamID, "Failed to store message")
		return
	}

	// Resolve attachment content for the new message (best effort; nil
	// resolver → skip, mirroring the disabled-attachment path).
	if s.resolver != nil && len(req.Attachments) > 0 {
		if parts, err := s.resolver.Resolve(r.Context(), userID, req.Attachments); err == nil && len(parts) > 0 {
			if raw, err := json.Marshal(toFormattedParts(parts)); err == nil {
				if err := s.repo.storeResolvedMessage(r.Context(), userMessageID, raw); err != nil {
					slog.Warn("dcs: store resolved message", "err", err)
				}
			}
		} else if err != nil {
			slog.Warn("dcs: resolve attachments", "err", err)
		}
	}

	// message_sent event for the user message (actor = sender).
	s.events.publish(r.Context(), chatID, EventChatMessageSent, chatMessageSentMeta{
		ChatID:          chatID,
		MessageID:       userMessageID,
		Role:            RoleUser,
		Model:           model,
		ActorUserID:     &userID,
		AttachmentCount: len(attachments),
	})

	if autoRename {
		s.spawnInitialChatRename(userID, chatID, streamID, req.Content)
	}

	// Resolved attachment chain for the whole chat (merged into the new user
	// message, mirroring build_chat_messages).
	var resolvedParts []ResolvedPart
	if blobs, err := s.repo.getResolvedMessageChain(r.Context(), chatID); err == nil {
		for _, blob := range blobs {
			resolvedParts = append(resolvedParts, fromFormattedParts(blob)...)
		}
	} else {
		slog.Warn("dcs: resolved message chain", "err", err, "chat", chatID)
	}

	// System prompt: toolset prompt + additional instructions + user memory.
	toolset := req.ToolSet.orDefault()
	system := s.toolsetPrompt(r.Context(), toolset)
	if req.AdditionalInstructions != nil && *req.AdditionalInstructions != "" {
		system += "\n" + *req.AdditionalInstructions
	}
	if s.memory != nil {
		if mem, err := s.memory.Memory(r.Context(), userID); err == nil && mem != "" {
			system += "\n\n<user_memory>\n" + mem + "\n</user_memory>"
		}
	}

	messages := append(prior, storedMessage{
		role:            RoleUser,
		content:         TextContent(req.Content),
		attachmentParts: resolvedParts,
	})

	var toolSpecs []ToolSpec
	if toolset == ToolSetAll && s.tools != nil {
		if specs, err := s.tools.Definitions(r.Context(), userID); err == nil {
			toolSpecs = specs
		}
	}

	// Detach the stream from the request lifecycle (Rust spawns a task that
	// outlives the HTTP response).
	streamCtx := context.WithoutCancel(r.Context())
	go s.runAIStream(streamCtx, chatID, userID, model, streamID, messageID, userMessageID, req, messages, system, toolSpecs)

	writeJSON(w, http.StatusOK, SendChatMessageResponse{
		StreamID:  streamID,
		MessageID: messageID,
		ChatID:    chatID,
	})
}

// runAIStream mirrors stream_and_save_message: emit the user message, stream
// provider parts (each forwarded to the hub + NATS), end with stream_end, then
// persist the accumulated assistant message and notify.
func (s *Service) runAIStream(ctx context.Context, chatID, userID, model, streamID, messageID, userMessageID string, req SendChatMessageRequest, messages []storedMessage, system string, toolSpecs []ToolSpec) {
	ctx, cancel := context.WithCancel(ctx)
	s.hub.open(streamID, chatID, cancel)

	// Cross-instance cancellation: subscribe to the cancel subject and reply
	// to stop requests so the stopper can detect a live stream (mirrors the
	// Redis pub/sub registry).
	var cancelSub *nats.Subscription
	if s.nc != nil && s.cfg.StreamCancelSubject != "" {
		subject := s.cfg.StreamCancelSubject + "." + streamID
		if sub, err := s.nc.Subscribe(subject, func(m *nats.Msg) {
			_ = m.Respond([]byte("1"))
			cancel()
		}); err == nil {
			cancelSub = sub
		}
	}
	defer func() {
		if cancelSub != nil {
			_ = cancelSub.Unsubscribe()
		}
	}()

	// Notify gateway-style consumers that a stream exists.
	s.publishNats(streamNotifySubject, []byte(fmt.Sprintf(
		`{"type":"created","entity_type":"chat","entity_id":%q,"stream_id":%q}`, chatID, streamID)))

	// The stream buffer stays replayable briefly after completion.
	defer func() {
		s.hub.finish(streamID)
		s.publishNats(streamNotifySubject, []byte(fmt.Sprintf(
			`{"type":"closed","entity_type":"chat","entity_id":%q,"stream_id":%q}`, chatID, streamID)))
		time.AfterFunc(streamBufferTTL, func() { s.hub.expire(streamID) })
	}()

	// First item: the user message that initiated the stream.
	userAtts := req.Attachments
	if userAtts == nil {
		userAtts = []Entity{}
	}
	s.emitStreamItem(streamID, ChatStreamUserMessage{
		Type:        "chat_user_message",
		StreamID:    streamID,
		ChatID:      chatID,
		MessageID:   userMessageID,
		Content:     req.Content,
		Attachments: userAtts,
	})

	// Idle watchdog: abort when no part arrives within the timeout (Rust uses
	// a 3-minute per-item timeout).
	idle := time.Duration(s.cfg.StreamIdleTimeoutSeconds) * time.Second
	if idle <= 0 {
		idle = 3 * time.Minute
	}
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > idle {
					slog.Error("dcs: AI stream idle timeout", "stream", streamID, "chat", chatID)
					cancel()
					return
				}
			}
		}
	}()

	var streamErr error
	parts, _, err := s.ai.StreamChat(ctx, &ChatRequest{
		Model:    model,
		Messages: toProviderMessages(messages, nil),
		System:   system,
		Stream:   true,
		Tools:    toolSpecs,
	}, func(part MessagePart) error {
		lastActivity.Store(time.Now().UnixNano())
		s.emitStreamItem(streamID, ChatStreamMessageResponse{
			Type:      "chat_message_response",
			StreamID:  streamID,
			MessageID: messageID,
			ChatID:    chatID,
			Content:   part,
		})
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		streamErr = err
	}
	wasCancelled := errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)

	if streamErr != nil {
		s.emitStreamItem(streamID, classifyStreamError(streamErr, streamID, model))
	}

	// stream_end always terminates the item stream.
	s.emitStreamItem(streamID, ChatStreamEnd{Type: "stream_end", StreamID: streamID})

	// Persist the accumulated assistant message (merged text/thinking +
	// synthetic "cancelled" tool errors for unanswered calls).
	finalParts := resolvePendingToolCalls(mergeConsecutiveParts(parts))
	var assistantText string
	if len(finalParts) > 0 {
		var b strings.Builder
		for _, p := range finalParts {
			if p.Type == PartText {
				b.WriteString(p.Text)
			}
		}
		assistantText = b.String()
		id, err := s.repo.createMessage(ctx, chatID, newMessage{
			id:        messageID,
			content:   PartsContent(finalParts),
			role:      RoleAssistant,
			model:     model,
			createdAt: time.Now(),
		})
		if err != nil {
			slog.Error("dcs: store assistant message", "err", err, "chat", chatID, "stream", streamID)
		} else {
			s.events.publish(ctx, chatID, EventChatMessageSent, chatMessageSentMeta{
				ChatID:    chatID,
				MessageID: id,
				Role:      RoleAssistant,
				Model:     model,
			})
		}
	}

	// Notification on completed (non-cancelled) replies only.
	if !wasCancelled && assistantText != "" && s.notifier != nil {
		s.notifier.NotifyChatResponse(ctx, chatID, messageID, assistantText, userID)
	}
}

// classifyStreamError mirrors StreamError::from(AgentStreamFailure): context
// overflow → model_context_overflow, provider failures → provider_error,
// everything else → internal_error.
func classifyStreamError(err error, streamID, model string) StreamError {
	var pe *ProviderError
	if errors.As(err, &pe) {
		if pe.isContextOverflow() {
			return StreamError{Type: "error", StreamError: StreamErrorModelContextOverflow, StreamID: streamID}
		}
		return StreamError{Type: "error", StreamError: StreamErrorProviderError, StreamID: streamID, Model: model}
	}
	return StreamError{Type: "error", StreamError: StreamErrorInternalError, StreamID: streamID}
}

// ---------------------------------------------------------------------------
// POST /stream/chat/message/stop
// ---------------------------------------------------------------------------

// stopChatStream mirrors stop.rs: edit access required; cancellation reaches
// the local registry and (via NATS request/reply) whichever instance runs the
// stream.
func (s *Service) stopChatStream(w http.ResponseWriter, r *http.Request) {
	caller, authed := auth.FromContext(r.Context())
	if !authed {
		writeTextErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req StopChatStreamRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if _, err := s.requireAccess(r.Context(), caller, authed, req.ChatID, AccessLevelEdit); err != nil {
		writeObjErr(w, http.StatusForbidden, "Insufficient permissions to stop chat stream")
		return
	}

	stopped := s.hub.cancel(req.StreamID)
	if s.nc != nil && s.cfg.StreamCancelSubject != "" {
		// Request/reply: a live remote subscriber responds, so `stopped`
		// reflects cross-instance hits too (mirrors redis publish received>0).
		subject := s.cfg.StreamCancelSubject + "." + req.StreamID
		if _, err := s.nc.Request(subject, []byte(req.StreamID), 500*time.Millisecond); err == nil {
			stopped = true
		}
	}
	writeJSON(w, http.StatusOK, StopChatStreamResponse{Stopped: stopped})
}

// ---------------------------------------------------------------------------
// GET /stream/chat/message/{stream_id} — SSE replay (Go-only addition)
// ---------------------------------------------------------------------------

// streamChatMessages replays the buffered stream items then follows live
// until stream_end. This replaces the connection_gateway websocket delivery
// for self-hosted clients/tests.
func (s *Service) streamChatMessages(w http.ResponseWriter, r *http.Request) {
	// Auth: only authenticated callers (verified JWT or internal key) may
	// replay a stream, and only with view access to the owning chat — the
	// Rust stack delivered these items over the authenticated
	// connection_gateway websocket, so the Go-only endpoint reproduces that
	// boundary.
	caller, authed := auth.FromContext(r.Context())
	if !authed {
		writeTextErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	streamID := chi.URLParam(r, "stream_id")
	chatID, ok := s.hub.chatOf(streamID)
	if !ok {
		writeObjErr(w, http.StatusNotFound, "stream not found")
		return
	}
	if _, err := s.requireAccess(r.Context(), caller, authed, chatID, AccessLevelView); err != nil {
		writeAccessErr(w, err)
		return
	}
	replay, ch, done := s.hub.subscribe(streamID)
	if replay == nil && ch == nil && !done {
		writeObjErr(w, http.StatusNotFound, "stream not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeObjErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeItem := func(item []byte) bool {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", item); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, item := range replay {
		if !writeItem(item) {
			return
		}
	}
	if done {
		return
	}
	defer s.hub.unsubscribe(streamID, ch)
	for {
		select {
		case <-r.Context().Done():
			return
		case item, ok := <-ch:
			if !ok {
				return
			}
			if !writeItem(item) {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// formatted attachment parts (attachment::FormattedParts serde round-trip)
// ---------------------------------------------------------------------------

// toFormattedParts serializes resolved parts as attachment::FormattedParts:
// [{"Text": ...} | {"Image": {"Base64": {"data": ...} | "StaticUrl": ...}}].
func toFormattedParts(parts []ResolvedPart) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.Kind == "image" {
			out = append(out, map[string]any{
				"Image": map[string]any{"Base64": map[string]string{"data": p.Data}},
			})
		} else {
			out = append(out, map[string]any{"Text": p.Text})
		}
	}
	return out
}

// fromFormattedParts decodes a persisted FormattedParts blob.
func fromFormattedParts(raw json.RawMessage) []ResolvedPart {
	var elems []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil
	}
	var out []ResolvedPart
	for _, el := range elems {
		if t, ok := el["Text"]; ok {
			var s string
			if json.Unmarshal(t, &s) == nil {
				out = append(out, ResolvedPart{Kind: "text", Text: s})
			}
			continue
		}
		if img, ok := el["Image"]; ok {
			var variants map[string]json.RawMessage
			if json.Unmarshal(img, &variants) != nil {
				continue
			}
			if b64, ok := variants["Base64"]; ok {
				var obj struct {
					Data string `json:"data"`
				}
				if json.Unmarshal(b64, &obj) == nil && obj.Data != "" {
					out = append(out, ResolvedPart{Kind: "image", MediaType: "image/webp", Data: obj.Data})
				}
			} else if url, ok := variants["StaticUrl"]; ok {
				var s string
				if json.Unmarshal(url, &s) == nil && s != "" {
					// Provider URL images are sent as image_url blocks.
					out = append(out, ResolvedPart{Kind: "image_url", Text: s})
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// auto rename (service::chat_renamer port)
// ---------------------------------------------------------------------------

const chatRenameSystemPrompt = `You generate short titles for AI chat conversations.

The user message you receive is raw input data: the first message in a chat.
Do not answer the user's question.
Do not ask follow-up questions.
Do not explain what you are doing.

Return only the chat title.
The title must be 2-6 words, concise, neutral, and specific to the user's topic.
Use title case.
No quotes, bullets, trailing punctuation, labels, or prefixes.`

// spawnInitialChatRename mirrors spawn_initial_chat_rename: generate a title
// on the fast (free) model, patch the chat, publish chat.updated, and fan a
// chat_renamed item out on the stream subject.
func (s *Service) spawnInitialChatRename(userID, chatID, streamID, firstMessage string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		name, err := s.generateChatName(ctx, firstMessage)
		if err != nil || name == "" {
			if err != nil {
				slog.Warn("dcs: auto-rename chat", "err", err, "chat", chatID)
			}
			return
		}
		if err := s.patchChat(ctx, userID, chatID, PatchChatRequest{Name: &name}); err != nil {
			slog.Warn("dcs: auto-rename patch", "err", err, "chat", chatID)
			return
		}
		// Mirrors the connection_gateway batch_send_to_entities call.
		payload, _ := json.Marshal(map[string]string{
			"type":      "chat_renamed",
			"stream_id": streamID,
			"chat_id":   chatID,
			"name":      name,
		})
		s.hub.publish(streamID, payload)
		s.publishNats(streamItemSubject+"."+streamID, payload)
	}()
}

func (s *Service) generateChatName(ctx context.Context, firstMessage string) (string, error) {
	text, _, err := s.ai.CompleteChat(ctx, &ChatRequest{
		Model: freeModel, // PredefinedModel::Fast → the cheap model
		Messages: []ProviderMessage{{
			Role:    RoleUser,
			Content: []ProviderBlock{{Type: "text", Text: fmt.Sprintf("<chat_first_message>\n%s\n</chat_first_message>\n\nGenerate the chat title now.", strings.TrimSpace(firstMessage))}},
		}},
		System:    chatRenameSystemPrompt,
		MaxTokens: 64,
	})
	if err != nil {
		return "", err
	}
	return cleanChatName(text), nil
}

// cleanChatName mirrors the Rust cleaner: trim quotes/whitespace, collapse
// internal whitespace, cap at 100 chars.
func cleanChatName(raw string) string {
	t := strings.TrimSpace(raw)
	t = strings.Trim(t, "\"'")
	t = strings.TrimSpace(t)
	t = strings.Join(strings.Fields(t), " ")
	r := []rune(t)
	if utf8.RuneCountInString(t) > maxChatNameLen {
		t = string(r[:maxChatNameLen])
	}
	return t
}
