package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Internal HTTP API — the non-NATS caller path, plus wire-compatible routes
// for crates/connection_gateway_client:
//
//	POST /internal/send                          {users, event, payload} → {delivered, receipts}
//	GET  /internal/presence?user=a&user=b        → {online}
//	GET  /internal/track/{entity_type}/{id}      → {users}  (track_entity_users)
//	POST /internal/track                         {entities:[…]} → {results:{type:id:[users]}}
//
// Rust client parity (same shapes as connection_gateway_models):
//
//	POST /message/send/{entity_type}/{id}        {message_type, message} → {receipts}
//	POST /message/batch_send                     {message_type, message, entities:[…]} → {receipts}
//	POST /message/batch_send_unique              {messages:[{entity, message_type, message_content}]} → {receipts}
//	GET  /track/{entity_type}/{id}               → [user_id, …]
//	GET  /health                                 → "healthy"

// entity mirrors model_entity::Entity's wire shape.
type entity struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
}

// messageReceipt mirrors connection_gateway_models::MessageReceipt.
type messageReceipt struct {
	UserID        string `json:"user_id"`
	DeliveryCount int    `json:"delivery_count"`
	Active        bool   `json:"active"`
}

type sendRequest struct {
	Users   []string        `json:"users"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

type sendResponse struct {
	Delivered []string         `json:"delivered"`
	Receipts  []messageReceipt `json:"receipts"`
}

type sendMessageBody struct {
	Message     json.RawMessage `json:"message"`
	MessageType string          `json:"message_type"`
}

type batchSendMessageBody struct {
	Message     json.RawMessage `json:"message"`
	Entities    []entity        `json:"entities"`
	MessageType string          `json:"message_type"`
}

type uniqueMessage struct {
	MessageContent json.RawMessage `json:"message_content"`
	Entity         entity          `json:"entity"`
	MessageType    string          `json:"message_type"`
}

type batchSendUniqueBody struct {
	Messages []uniqueMessage `json:"messages"`
}

// internalMux builds the internal-only HTTP handler. Health endpoints stay
// outside the key check so liveness probes keep working even when the API
// is intentionally locked down; every other route requires the key.
func (s *Server) internalMux() http.Handler {
	protected := http.NewServeMux()
	protected.HandleFunc("POST /internal/send", s.handleInternalSend)
	protected.HandleFunc("GET /internal/presence", s.handlePresence)
	protected.HandleFunc("GET /internal/track/{entity_type}/{entity_id}", s.handleTrackJSON)
	protected.HandleFunc("POST /internal/track", s.handleTrackBatch)

	// connection_gateway_client-compatible routes.
	protected.HandleFunc("POST /message/send/{entity_type}/{entity_id}", s.handleSendToEntity)
	protected.HandleFunc("POST /message/batch_send", s.handleBatchSend)
	protected.HandleFunc("POST /message/batch_send_unique", s.handleBatchSendUnique)
	protected.HandleFunc("GET /track/{entity_type}/{entity_id}", s.handleTrackCompat)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("/", s.requireInternalKey(protected))
	return mux
}

// requireInternalKey checks x-internal-auth-key — the same header
// connection_gateway_client sends. When no key is configured the internal
// API fails closed: only ENVIRONMENT=dev (or an unset env, i.e. local
// development) leaves it open; anything else rejects every request so a
// missing key can never silently expose the internal surface.
func (s *Server) requireInternalKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.InternalAPIKey == "" {
			if s.cfg.Env == "" || s.cfg.Env == "dev" {
				next.ServeHTTP(w, r)
				return
			}
			slog.Warn("gateway: internal API request rejected — no GATEWAY_INTERNAL_API_KEY configured",
				"path", r.URL.Path, "env", s.cfg.Env)
			http.Error(w, "internal api disabled: GATEWAY_INTERNAL_API_KEY unset", http.StatusServiceUnavailable)
			return
		}
		got := r.Header.Get("x-internal-auth-key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.InternalAPIKey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("healthy"))
}

// clientFrame is the exact websocket wire shape the Rust
// connection_gateway emitted — model/message.rs `Message { type, data }`
// where `data` is a JSON document serialized *as a string*. The web client
// (apps/web service-connection/websocket-payload.ts) reads `frame.type` and
// `frame.data`, JSON-parsing `data` when it is a string — and the stream
// handlers parse it unconditionally, so `data` must always be a string.
type clientFrame struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// wireMessage builds a clientFrame whose data is the given JSON document.
func wireMessage(msgType string, data json.RawMessage) ([]byte, error) {
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	return json.Marshal(clientFrame{Type: msgType, Data: string(data)})
}

// normalizeClientFrame converts a bus payload into the websocket wire shape.
// Producers publish either an events.Envelope ({type, data: <object>}) or a
// bare {type, ...} document; both become a legacy {type, data: <string>}
// frame. Payloads without a string `type` pass through untouched.
func normalizeClientFrame(payload []byte) []byte {
	var probe struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil || probe.Type == "" {
		return payload
	}
	if probe.Data == nil {
		// Bare {type, ...} message (e.g. notification_status_updated): the
		// whole document is the payload the client expects under `data`.
		out, err := wireMessage(probe.Type, payload)
		if err != nil {
			return payload
		}
		return out
	}
	// Envelope-shaped: hoist `data`. If it is already a string (the Rust
	// bus carried Message.data verbatim), keep it as-is.
	var s string
	if err := json.Unmarshal(probe.Data, &s); err == nil {
		out, err := json.Marshal(clientFrame{Type: probe.Type, Data: s})
		if err != nil {
			return payload
		}
		return out
	}
	out, err := wireMessage(probe.Type, probe.Data)
	if err != nil {
		return payload
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxInternalBodyBytes caps internal API request bodies — axum's
// DefaultBodyLimit on the Rust service was 2 MiB.
const maxInternalBodyBytes = 2 << 20

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxInternalBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// handleInternalSend — POST /internal/send
// {users:[…], event:"…", payload:{…}} → delivers the event as an Envelope to
// each user's connections (via NATS fanout) and reports which users have at
// least one live connection — the online-detection signal the notification
// service used MessageReceipt.active for.
func (s *Server) handleInternalSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Users) == 0 || req.Event == "" {
		http.Error(w, "users and event are required", http.StatusBadRequest)
		return
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage("null")
	}

	// One frame for every recipient — the Rust service sent the same
	// Message {type, data} to all of them.
	payload, err := wireMessage(req.Event, req.Payload)
	if err != nil {
		http.Error(w, "encode message: "+err.Error(), http.StatusBadRequest)
		return
	}

	receipts := make([]messageReceipt, 0, len(req.Users))
	delivered := make([]string, 0, len(req.Users))
	for _, userID := range req.Users {
		s.deliverRaw([]string{userID}, payload)

		n := s.liveConnCount(r.Context(), userID)
		receipts = append(receipts, messageReceipt{
			UserID:        userID,
			DeliveryCount: n,
			Active:        n > 0,
		})
		if n > 0 {
			delivered = append(delivered, userID)
		}
	}
	writeJSON(w, http.StatusOK, sendResponse{Delivered: delivered, Receipts: receipts})
}

// handlePresence — GET /internal/presence?user=a&user=b → {"online":[…]}
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	users := r.URL.Query()["user"]
	online, err := s.reg.OnlineUsers(r.Context(), users)
	if err != nil {
		slog.Warn("gateway: presence lookup failed, using local hub", "err", err)
		online = s.hub.localOnline(users)
	}
	writeJSON(w, http.StatusOK, map[string]any{"online": online})
}

// handleTrackJSON — GET /internal/track/{entity_type}/{entity_id} → {"users":[…]}
func (s *Server) handleTrackJSON(w http.ResponseWriter, r *http.Request) {
	users, ok := s.entityUsers(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// handleTrackCompat — GET /track/{entity_type}/{entity_id} → [user_id, …]
// (the bare-array shape connection_gateway_client.track_entity_users parses)
func (s *Server) handleTrackCompat(w http.ResponseWriter, r *http.Request) {
	users, ok := s.entityUsers(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) entityUsers(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	entityType, entityID := r.PathValue("entity_type"), r.PathValue("entity_id")
	users, err := s.resolveEntityUsers(r.Context(), entityType, entityID)
	if err != nil {
		http.Error(w, "unable to get entries by entity", http.StatusInternalServerError)
		return nil, false
	}
	if users == nil {
		users = []string{}
	}
	return users, true
}

// resolveEntityUsers ports get_users_for_entity: a "user" entity resolves to
// itself when online; any other entity resolves via tracked connections.
func (s *Server) resolveEntityUsers(ctx context.Context, entityType, entityID string) ([]string, error) {
	if entityType == "user" {
		online, err := s.reg.OnlineUsers(ctx, []string{entityID})
		if err != nil {
			return nil, err
		}
		return online, nil
	}
	return s.reg.EntityUsers(ctx, entityType, entityID)
}

// handleTrackBatch — POST /internal/track {entities:[{entity_type,entity_id}]}
// → {"results":{"type:id":[users]}} — batch equivalent of track_entity_users.
func (s *Server) handleTrackBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Entities []entity `json:"entities"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	results := make(map[string][]string, len(req.Entities))
	for _, e := range req.Entities {
		users, err := s.resolveEntityUsers(r.Context(), e.EntityType, e.EntityID)
		if err != nil {
			http.Error(w, "unable to get entries by entity", http.StatusInternalServerError)
			return
		}
		results[e.EntityType+":"+e.EntityID] = users
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// liveConnCount reports how many live connections a user has, preferring the
// shared registry (covers all replicas) and degrading to the local hub.
func (s *Server) liveConnCount(ctx context.Context, userID string) int {
	conns, err := s.reg.Connections(ctx, userID)
	if err != nil {
		slog.Debug("gateway: registry conn lookup failed, using local hub",
			"user_id", userID, "err", err)
		return len(s.hub.localConns(userID))
	}
	return len(conns)
}

// entityMessage ports send_message_to_entity: resolve the entity to users,
// wrap the payload in a {type, data} client frame, and deliver.
func (s *Server) entityMessage(r *http.Request, e entity, messageType string, message json.RawMessage) ([]messageReceipt, error) {
	userIDs, err := s.resolveEntityUsers(r.Context(), e.EntityType, e.EntityID)
	if err != nil {
		return nil, err
	}
	payload, err := wireMessage(messageType, message)
	if err != nil {
		return nil, err
	}
	s.deliverRaw(userIDs, payload)

	byUser := make(map[string]int, len(userIDs))
	for _, u := range userIDs {
		byUser[u] = s.liveConnCount(r.Context(), u)
	}
	receipts := make([]messageReceipt, 0, len(byUser))
	for u, n := range byUser {
		receipts = append(receipts, messageReceipt{UserID: u, DeliveryCount: n, Active: n > 0})
	}
	return receipts, nil
}

func (s *Server) handleSendToEntity(w http.ResponseWriter, r *http.Request) {
	var body sendMessageBody
	if !decodeJSON(w, r, &body) {
		return
	}
	e := entity{EntityType: r.PathValue("entity_type"), EntityID: r.PathValue("entity_id")}
	receipts, err := s.entityMessage(r, e, body.MessageType, body.Message)
	if err != nil {
		http.Error(w, "unable to send message", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts})
}

func (s *Server) handleBatchSend(w http.ResponseWriter, r *http.Request) {
	var body batchSendMessageBody
	if !decodeJSON(w, r, &body) {
		return
	}
	var receipts []messageReceipt
	for _, e := range body.Entities {
		rs, err := s.entityMessage(r, e, body.MessageType, body.Message)
		if err != nil {
			http.Error(w, "unable to send message", http.StatusInternalServerError)
			return
		}
		receipts = append(receipts, rs...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts})
}

func (s *Server) handleBatchSendUnique(w http.ResponseWriter, r *http.Request) {
	var body batchSendUniqueBody
	if !decodeJSON(w, r, &body) {
		return
	}
	var receipts []messageReceipt
	for _, m := range body.Messages {
		rs, err := s.entityMessage(r, m.Entity, m.MessageType, m.MessageContent)
		if err != nil {
			http.Error(w, "unable to send message", http.StatusInternalServerError)
			return
		}
		receipts = append(receipts, rs...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts})
}
