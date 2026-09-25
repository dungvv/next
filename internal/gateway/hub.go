package gateway

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// conn is one live websocket connection owned by this gateway instance.
// Port of model/connection.rs `Connection { sender, abort_handle }`.
type conn struct {
	id     string
	userID string
	ws     *websocket.Conn

	// send is the bounded outbound queue. A producer that finds it full
	// drops the connection — same as the Rust `try_send` → remove_connection.
	send chan []byte
	// done is closed when the connection is removed; the write loop exits.
	done chan struct{}

	// hbPending coalesces async heartbeat writes so a backed-up registry
	// cannot spawn an unbounded goroutine per inbound ping.
	hbPending atomic.Bool
}

// Hub tracks this instance's live connections. It is the in-process half of
// the Rust ConnectionManager: the authoritative "which sockets do we hold"
// map used for NATS fanout. Cross-instance presence lives in Registry.
type Hub struct {
	bufSize int

	mu     sync.RWMutex
	byID   map[string]*conn
	byUser map[string]map[string]*conn
}

func newHub(bufSize int) *Hub {
	if bufSize <= 0 {
		bufSize = 256
	}
	return &Hub{
		bufSize: bufSize,
		byID:    make(map[string]*conn),
		byUser:  make(map[string]map[string]*conn),
	}
}

func (h *Hub) add(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byID[c.id] = c
	if h.byUser[c.userID] == nil {
		h.byUser[c.userID] = make(map[string]*conn)
	}
	h.byUser[c.userID][c.id] = c
}

// remove detaches the connection and signals its write loop to stop.
// Idempotent: a refused send and a reader exit both end here.
func (h *Hub) remove(id string) {
	h.mu.Lock()
	c, ok := h.byID[id]
	if ok {
		delete(h.byID, id)
		if m := h.byUser[c.userID]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(h.byUser, c.userID)
			}
		}
	}
	h.mu.Unlock()
	if ok {
		close(c.done)
	}
}

func (h *Hub) has(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.byID[id]
	return ok
}

// offer queues a message for one connection. Never blocks: a full queue
// means the consumer is a whole buffer behind, so the connection is
// dropped and the client reconnects (mirrors ConnectionManager.send_message).
func (h *Hub) offer(id string, msg []byte) bool {
	h.mu.RLock()
	c, ok := h.byID[id]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case c.send <- msg:
		return true
	default:
		slog.Warn("gateway: outbound queue full, dropping connection",
			"connection_id", id, "user_id", c.userID, "queue_depth", len(c.send))
		h.remove(id)
		_ = c.ws.Close(websocket.StatusPolicyViolation, "slow consumer")
		return false
	}
}

// sendToUser delivers msg to every local connection of userID and returns
// how many took it.
func (h *Hub) sendToUser(userID string, msg []byte) int {
	h.mu.RLock()
	conns := make([]*conn, 0, len(h.byUser[userID]))
	for _, c := range h.byUser[userID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	n := 0
	for _, c := range conns {
		if h.offer(c.id, msg) {
			n++
		}
	}
	return n
}

// sendToConns delivers msg to each named local connection; returns how many
// took it. Used for entity-scoped delivery where the registry resolves the
// connection ids.
func (h *Hub) sendToConns(connIDs []string, msg []byte) int {
	n := 0
	for _, id := range connIDs {
		if h.offer(id, msg) {
			n++
		}
	}
	return n
}

// localConns returns this instance's connection ids for a user.
func (h *Hub) localConns(userID string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.byUser[userID]))
	for id := range h.byUser[userID] {
		out = append(out, id)
	}
	return out
}

// localOnline filters userIDs down to those with at least one local conn.
func (h *Hub) localOnline(userIDs []string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(userIDs))
	for _, u := range userIDs {
		if len(h.byUser[u]) > 0 {
			out = append(out, u)
		}
	}
	return out
}

func (h *Hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byID)
}
