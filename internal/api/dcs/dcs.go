// Package dcs implements the document cognition service port: the AI chat
// API (create/list/patch/delete/copy/restore chats, message history, SSE
// streaming of Anthropic completions) plus the small utility endpoints
// (citations, previews, attachments, id_mapping, structured completion,
// OpenAI completions proxy).
//
// Sources ported:
//   - services/document_cognition_service/src/api/{chats,stream,…}
//   - crates/chat (chat CRUD, sanitized lifecycle events)
//   - crates/agent (message → provider conversion, stream accumulation)
//
// Deliberately stubbed (interfaces + TODOs, see tools.go): AI toolsets, MCP
// connector wiring, agent-context, memory, notifications, and attachment
// resolution (the sync service fetch is replaced by a Resolver interface).
package dcs

import (
	"context"
	"fmt"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// Deps are the external dependencies the dcs router needs. Constructed once in
// api.go and shared by every mount (root, /cognition, versioned).
type Deps struct {
	Pool *pgxpool.Pool
	JS   jetstream.JetStream // may be nil when NATS is unavailable
	NC   *nats.Conn          // may be nil
	Cfg  Config

	// AI is the chat completion provider (Anthropic by default). Swap in
	// tests via WithAIClient.
	AI AIClient
	// Resolver resolves message attachments into provider-ready content
	// parts. Nil disables attachment resolution (attachments are persisted
	// but not expanded into the prompt). TODO(tools): port the sync-service
	// attachment fetcher.
	Resolver AttachmentResolver
	// Tools exposes per-tool execution for the /chats/{id}/tool/* endpoints.
	// Nil → the endpoints return 501. TODO(tools): port ai_tools + MCP.
	Tools ToolRunner
	// Memory supplies the per-user memory injected into the system prompt.
	// Nil disables it. TODO: port memory service.
	Memory MemoryProvider
	// Notifier emits "chat response ready" notifications. Nil disables it.
	// TODO: port notification_ingress notification.
	Notifier ChatNotifier
}

// Service is the dcs handler set.
type Service struct {
	repo     *repo
	q        *macrodb.Queries
	cfg      Config
	events   *Publisher
	nc       *nats.Conn
	ai       AIClient
	resolver AttachmentResolver
	tools    ToolRunner
	memory   MemoryProvider
	notifier ChatNotifier

	// hub tracks in-flight AI streams: buffered items for replay/SSE, local
	// cancellation, and subscriber fan-out (replaces the Redis durable stream
	// + ai_stream_registry in the Rust stack).
	hub *streamHub
}

// New builds the Service from Deps.
func New(d Deps) (*Service, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("dcs: pool is required")
	}
	ai := d.AI
	if ai == nil {
		ai = NewAnthropicClient(d.Cfg.AnthropicAPIKey, d.Cfg.AnthropicBaseURL, d.Cfg.AnthropicVersion, d.Cfg.AnthropicMaxTokens)
	}
	r := newRepo(d.Pool)
	return &Service{
		repo:     r,
		q:        r.q,
		cfg:      d.Cfg,
		events:   &Publisher{JS: d.JS},
		nc:       d.NC,
		ai:       ai,
		resolver: d.Resolver,
		tools:    d.Tools,
		memory:   d.Memory,
		notifier: d.Notifier,
		hub:      newStreamHub(),
	}, nil
}

// EnsureStreams provisions the macro.chats event stream (call once at boot).
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	_, err := natsx.EnsureEventStream(ctx, js, events.StreamChats, []string{events.StreamChats + ".>"})
	return err
}

// Register mounts every DCS route on r. Auth middleware is applied by the
// caller (the combined api router wraps this in OptionalMiddleware and each
// handler enforces its own caller requirements, mirroring the Rust extractor
// model where GET /chats/{id} permits anonymous public-link viewers).
func (s *Service) Register(r chi.Router) {
	// --- chats (chat crate router + DCS history routes) ------------------
	r.Route("/chats", func(r chi.Router) {
		r.Post("/", s.createChatHandler)
		r.Get("/", s.listChats)
		r.Get("/history/{chat_id}", s.getChatHistory)
		r.Post("/history_batch_messages", s.getChatHistoryBatchMessages)
		r.Get("/{chat_id}", s.getChat)
		r.Patch("/{chat_id}", s.patchChatHandler)
		r.Delete("/{chat_id}", s.deleteChat)
		r.Delete("/{chat_id}/permanent", s.permanentlyDeleteChat)
		r.Post("/{chat_id}/copy", s.copyChatHandler)
		r.Put("/{chat_id}/revert_delete", s.revertDeleteChat)
		r.Get("/{chat_id}/permissions", s.getChatPermissions)
		// Tool endpoints — stubs pending the ai_tools/MCP port.
		r.Post("/{chat_id}/tool/update", s.updateToolCall)
		r.Post("/{chat_id}/tool/response/update", s.updateToolResponse)
		r.Post("/{chat_id}/tool/call", s.callTool)
		r.Post("/{chat_id}/tool/reject", s.rejectToolCall)
	})

	// --- streaming -------------------------------------------------------
	r.Route("/stream", func(r chi.Router) {
		r.Post("/chat/message", s.sendChatMessage)
		r.Post("/chat/message/stop", s.stopChatStream)
		// Go-only addition: direct SSE replay of a stream's buffered items.
		// In the Rust stack the connection_gateway service fans stream items
		// out to websocket clients from Redis; self-hosted deployments can
		// point their gateway at the NATS subjects, and this endpoint gives
		// clients/tests a direct way to consume a stream.
		r.Get("/chat/message/{stream_id}", s.streamChatMessages)
	})

	// --- misc ------------------------------------------------------------
	r.Post("/structured-completion", s.structuredCompletion)
	r.Post("/chat/completions", s.chatCompletions)
	r.Get("/attachments/{attachment_id}/chats", s.getChatsForAttachment)
	r.Get("/citations/{id}", s.getCitation)
	r.Post("/preview", s.getBatchPreview)
	r.Post("/id_mapping/{source_id}", s.createIDMappingHandler)
	r.Get("/id_mapping/{source_id}", s.getIDMappingHandler)
}
