// The runtime gateway: where user-run harnesses dial in to serve their
// agents — the port of agent_harness::inbound::runtime_gateway.
//
// GET /runtime/ws authenticated with x-macro-harness-token. One connection
// per harness; sessions bind to the connection as work arrives. The token is
// the whole gate, verified against harness_tokens.token_hash (SHA-256 of the
// raw token) on a live harness row.
package agentharness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/httpx"
)

// HeaderHarnessToken is the harness credential header.
const HeaderHarnessToken = "x-macro-harness-token"

// Registry tracks live harness connections — the port of
// outbound::runtime_registry. Last dial wins: a runtime that redials has
// lost its old socket whether or not this side noticed.
type Registry struct {
	mu    sync.Mutex
	conns map[string]*harnessConn // harness id -> connection
	binds map[string]string       // session id -> harness id
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{conns: map[string]*harnessConn{}, binds: map[string]string{}}
}

type harnessConn struct {
	conn *websocket.Conn
	mu   sync.Mutex // serialize writes
}

func (r *Registry) attach(harnessID string, conn *websocket.Conn) {
	r.mu.Lock()
	if old := r.conns[harnessID]; old != nil {
		_ = old.conn.Close(websocket.StatusGoingAway, "replaced by a new dial")
	}
	r.conns[harnessID] = &harnessConn{conn: conn}
	r.mu.Unlock()
}

func (r *Registry) detach(harnessID string, conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.conns[harnessID]; cur != nil && cur.conn == conn {
		delete(r.conns, harnessID)
	}
}

// BindSession routes a session's outbound frames to a harness.
func (r *Registry) BindSession(sessionID, harnessID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.binds[sessionID] = harnessID
}

// Send forwards one ToRuntime envelope to the harness bound to the session.
// Returns false when no live connection serves it.
func (r *Registry) Send(sessionID string, frame []byte) bool {
	r.mu.Lock()
	harnessID, ok := r.binds[sessionID]
	var hc *harnessConn
	if ok {
		hc = r.conns[harnessID]
	}
	r.mu.Unlock()
	if hc == nil {
		return false
	}
	hc.mu.Lock()
	defer hc.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hc.conn.Write(ctx, websocket.MessageText, frame) == nil
}

// lookupHarness resolves a raw harness token to its harness id: SHA-256 of
// the token against harness_tokens.token_hash on an unrevoked token of a
// live harness — the port of pg_harness_authorization's lookup.
func (s *Service) lookupHarness(ctx context.Context, token string) (string, error) {
	hash := sha256.Sum256([]byte(token))
	var harnessID string
	err := s.repo.db.QueryRow(ctx, `
		SELECT ht.harness_id::text
		FROM harness_tokens ht
		JOIN harnesses h ON h.id = ht.harness_id
		WHERE ht.token_hash = $1
		  AND ht.revoked_at IS NULL
		  AND h.deleted_at IS NULL`, hash[:]).Scan(&harnessID)
	if err != nil {
		return "", err
	}
	// Bookkeeping parity: last_used_at marks the credential as seen.
	_, _ = s.repo.db.Exec(ctx,
		`UPDATE harness_tokens SET last_used_at = now() WHERE token_hash = $1`, hash[:])
	return harnessID, nil
}

// runtimeWSHandler serves GET /runtime/ws.
func (s *Service) runtimeWSHandler(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get(HeaderHarnessToken)
	if token == "" {
		httpx.Error(w, http.StatusUnauthorized, "missing harness token")
		return
	}
	harnessID, err := s.lookupHarness(r.Context(), token)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "invalid harness token")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Harnesses dial from arbitrary origins (a user's laptop, a remote
		// box); the token is the gate, not Origin.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	s.registry.attach(harnessID, conn)
	slog.Info("agentharness: a runtime dialed in", "harness", harnessID)
	defer func() {
		s.registry.detach(harnessID, conn)
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
	}()

	// Read loop: ToServer envelopes. Minimal port — ACP frames and lifecycle
	// events are recorded in the owning session's log when the frame names a
	// sessionId; full session binding/fanout is TODO(acp-bridge).
	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var env struct {
			Type      string          `json:"type"`
			Event     string          `json:"event"`
			SessionID string          `json:"sessionId"`
			Raw       json.RawMessage `json:"-"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			slog.Warn("agentharness: dropping undecodable runtime frame", "err", err)
			continue
		}
		if env.SessionID != "" {
			if err := s.repo.AppendLog(r.Context(), uuid.NewString(), env.SessionID,
				nil, "to_server", data); err != nil {
				slog.Warn("agentharness: failed to log runtime frame",
					"session", env.SessionID, "err", err)
			}
		}
		if env.Type == "event" {
			s.handleRuntimeEvent(r.Context(), env.SessionID, env.Event)
		}
	}
}

// handleRuntimeEvent mirrors the lifecycle-event side of the Rust gateway:
// the session status follows acp_ready / disconnected / reload_required.
func (s *Service) handleRuntimeEvent(ctx context.Context, sessionID, event string) {
	if sessionID == "" {
		return
	}
	var status string
	var name *string
	switch event {
	case "disconnected":
		status = "disconnected"
	default:
		status = "event"
		name = &event
	}
	if err := s.repo.SetStatus(ctx, sessionID, status, name); err != nil {
		slog.Warn("agentharness: failed to record runtime event",
			"session", sessionID, "event", event, "err", err)
	}
}

// sendToSidecar dials a managed sandbox's ACP sidecar over the container
// network and writes one envelope. The sidecar speaks bare ACP frames; the
// envelope's acp payload is what it carries — TODO(acp-bridge) for the full
// protocol pump.
func sendToSidecar(ctx context.Context, containerName string, frame []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx,
		"ws://"+containerName+":8700/ws", nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	return conn.Write(ctx, websocket.MessageText, frame)
}
