package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// clientMessage is the inbound control channel from the web client.
// Port of model/websocket.rs ToWebsocketMessage:
//
//	{"type":"track_entity","entity_type":"document","entity_id":"x","action":"open"}
//	{"type":"stream_events","entity_type":"document","entity_id":"x"}
//	"ping" (bare text) → "pong"
type clientMessage struct {
	Type       string `json:"type"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Action     string `json:"action"` // open | ping | close
}

// userTrackingChange mirrors service/tracker.rs UserTrackingChange — sent to
// an entity's users whenever tracking membership changes.
type userTrackingChange struct {
	EntityType string   `json:"entity_type"`
	EntityID   string   `json:"entity_id"`
	UserIDs    []string `json:"user_ids"`
}

const (
	pingMessage = "ping"
	pongMessage = "pong"
)

// slowWriteWarn mirrors SLOW_WEBSOCKET_OPERATION_THRESHOLD.
const slowWriteWarn = time.Second

// maxWSReadBytes caps inbound websocket frames. The Rust service ran axum's
// defaults (max_message_size = 64 MiB); control messages are tiny, so the
// cap is only a safety bound.
const maxWSReadBytes = 64 << 20

// handleWS is the public websocket endpoint. Mounted at "/", "/connection-gateway",
// and "/connection-gateway/" (the Rust router exposed all three).
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	userID, err := s.verifier.Verify(r.Context(), token)
	if err != nil {
		slog.Debug("gateway: websocket auth failed", "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	opts := &websocket.AcceptOptions{}
	if patterns := s.cfg.wsOriginPatterns(); len(patterns) > 0 {
		opts.OriginPatterns = patterns
	} else {
		opts.InsecureSkipVerify = true // dev: accept any origin
	}
	ws, err := websocket.Accept(w, r, opts)
	if err != nil {
		slog.Warn("gateway: websocket accept failed", "err", err)
		return
	}
	ws.SetReadLimit(maxWSReadBytes)

	// The socket lives until the client disconnects OR the service shuts
	// down (http.Server.Shutdown does not interrupt hijacked connections).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if s.lifeCtx != nil {
		stop := context.AfterFunc(s.lifeCtx, cancel)
		defer stop()
	}

	connID := uuid.NewString()
	c := &conn{
		id:     connID,
		userID: userID,
		ws:     ws,
		send:   make(chan []byte, s.cfg.SendBuffer),
		done:   make(chan struct{}),
	}
	s.hub.add(c)
	if err := s.reg.Add(r.Context(), userID, connID); err != nil {
		slog.Warn("gateway: failed to register connection", "err", err, "conn", connID)
	}
	slog.Info("gateway: connection established", "user_id", userID, "conn", connID,
		"total", s.hub.count())

	go s.writeLoop(ctx, c)
	s.readLoop(ctx, c)

	// Cleanup: whichever loop ended first brings the other down.
	s.hub.remove(connID)
	ws.CloseNow()
	rmCtx, rmCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = s.reg.Remove(rmCtx, userID, connID)
	rmCancel()
	if err != nil {
		slog.Warn("gateway: failed to deregister connection", "err", err, "conn", connID)
	}
	slog.Info("gateway: connection closed", "user_id", userID, "conn", connID,
		"total", s.hub.count())
}

// writeLoop drains the bounded outbound queue to the socket.
// Port of the Rust `forwarder` task.
func (s *Server) writeLoop(ctx context.Context, c *conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case msg := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
			start := time.Now()
			err := c.ws.Write(wctx, websocket.MessageText, msg)
			cancel()
			if d := time.Since(start); d > slowWriteWarn {
				slog.Warn("gateway: websocket write blocked",
					"conn", c.id, "write_ms", d.Milliseconds(),
					"queue_depth", len(c.send))
			}
			if err != nil {
				slog.Debug("gateway: websocket write failed, dropping conn",
					"conn", c.id, "err", err)
				c.ws.CloseNow()
				s.hub.remove(c.id)
				return
			}
		}
	}
}

// readLoop handles inbound control messages until the socket closes.
// Port of handle_websocket_stream / handle_message.
func (s *Server) readLoop(ctx context.Context, c *conn) error {
	for {
		mt, data, err := c.ws.Read(ctx)
		if err != nil {
			status := websocket.CloseStatus(err)
			if status != websocket.StatusNormalClosure &&
				status != websocket.StatusGoingAway &&
				status != websocket.StatusAbnormalClosure &&
				!errors.Is(err, context.Canceled) {
				slog.Warn("gateway: websocket read ended abnormally",
					"conn", c.id, "user_id", c.userID, "err", err)
			}
			return err
		}
		if mt != websocket.MessageText {
			continue
		}
		s.handleClientMessage(ctx, c, data)
	}
}

func (s *Server) handleClientMessage(ctx context.Context, c *conn, data []byte) {
	text := strings.TrimSpace(string(data))

	// Bare "ping" — the heartbeat. Refresh registry liveness, reply "pong"
	// (same wire contract as the Rust service).
	if text == pingMessage {
		s.heartbeat(c)
		select {
		case c.send <- []byte(pongMessage):
		case <-c.done:
		}
		return
	}

	var msg clientMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		slog.Debug("gateway: unparseable client message", "conn", c.id, "err", err)
		return
	}

	// Any inbound message proves liveness.
	s.heartbeat(c)

	switch msg.Type {
	case "track_entity":
		s.handleTrackEntity(ctx, c, msg)
	case "stream_events":
		// TODO: port stream_manager (RedisPostgresStreamManager) — the Rust
		// service subscribed the connection to stream items/events for the
		// entity. No-op for now; clients receive realtime fanout regardless.
		slog.Debug("gateway: stream_events not implemented",
			"conn", c.id, "entity", msg.EntityType+":"+msg.EntityID)
	default:
		slog.Debug("gateway: unknown client message type", "type", msg.Type)
	}
}

// heartbeat refreshes registry liveness. It runs on a detached context in a
// goroutine — the read loop must never block on Redis/Postgres (a slow store
// would stall every inbound message, and Rust did the equivalent write via
// an async worker). A per-connection flag coalesces pings so a backed-up
// store can't spawn unbounded goroutines.
func (s *Server) heartbeat(c *conn) {
	if !c.hbPending.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.hbPending.Store(false)
		hctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.reg.Heartbeat(hctx, c.userID, c.id); err != nil {
			slog.Debug("gateway: heartbeat failed", "conn", c.id, "err", err)
		}
	}()
}

// handleTrackEntity ports tracker.rs track_entity: open registers the conn
// on the entity, ping refreshes it, close removes it; membership changes
// broadcast user_tracking_change to the entity's users.
func (s *Server) handleTrackEntity(ctx context.Context, c *conn, msg clientMessage) {
	if msg.EntityType == "" || msg.EntityID == "" {
		return
	}
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	switch msg.Action {
	case "open":
		err = s.reg.TrackEntity(rctx, c.id, c.userID, msg.EntityType, msg.EntityID)
	case "close":
		err = s.reg.UntrackEntity(rctx, c.id, msg.EntityType, msg.EntityID)
	case "ping":
		// Rust update_last_entity_ping propagates to the top-level
		// connection's last_ping — refresh both records.
		err = s.reg.PingEntity(rctx, c.id, c.userID, msg.EntityType, msg.EntityID)
	default:
		slog.Debug("gateway: unknown track action", "action", msg.Action)
		return
	}
	if err != nil {
		slog.Debug("gateway: track_entity failed", "conn", c.id, "err", err)
		return
	}

	// Frecency ingest: every track action records an event row; the
	// aggregator worker folds them into frecency_aggregates (Rust
	// tracker.rs → EventIngestorImpl → frecency_events).
	if s.frec != nil {
		if err := s.frec.RecordEvent(rctx, c.userID, msg.EntityType, msg.EntityID,
			msg.Action, c.id, time.Now().UTC()); err != nil {
			slog.Debug("gateway: frecency record failed", "conn", c.id, "err", err)
		}
	}

	if msg.Action == "open" || msg.Action == "close" {
		s.notifyTrackingChange(rctx, msg.EntityType, msg.EntityID)
	}
}

// notifyTrackingChange ports tracker.rs notify_tracking_change: resolve the
// entity's active users and push a user_tracking_change event to each.
func (s *Server) notifyTrackingChange(ctx context.Context, entityType, entityID string) {
	users, err := s.reg.EntityUsers(ctx, entityType, entityID)
	if err != nil {
		slog.Debug("gateway: entity users lookup failed", "err", err)
		return
	}
	data, err := json.Marshal(userTrackingChange{
		EntityType: entityType, EntityID: entityID, UserIDs: users,
	})
	if err != nil {
		return
	}
	payload, err := wireMessage("user_tracking_change", data)
	if err != nil {
		return
	}
	s.deliverRaw(users, payload)
}
