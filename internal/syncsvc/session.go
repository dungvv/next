package syncsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// sessionManager is the Go analogue of the DocumentSyncSession Durable
// Object: one live docSession per document_id, serialized by a PG advisory
// lock instead of DO single-threading, with ephemeral state (sockets,
// awareness) in process memory.
type sessionManager struct {
	store   *pgStore
	factory engineFactory
	cfg     syncConfig
	log     *slog.Logger

	mu       sync.Mutex
	sessions map[string]*docSession
	closing  bool
}

func newSessionManager(store *pgStore, factory engineFactory, cfg syncConfig, log *slog.Logger) *sessionManager {
	return &sessionManager{
		store:    store,
		factory:  factory,
		cfg:      cfg,
		log:      log,
		sessions: map[string]*docSession{},
	}
}

// docSession is the live state for one document — equivalent to the Rust
// DocumentSyncSession + DocumentState, minus workerd.
type docSession struct {
	id     string
	mgr    *sessionManager
	lock   *lockConn
	engine docEngine

	mu      sync.Mutex
	sockets map[*peerConn]struct{}
	refs    int // in-flight users (ws conns + HTTP ops)
	closing bool
	onIdle  func(*docSession) // invoked when refs hits 0
}

// peerConn is one websocket attachment (Rust `sockets: HashMap<*ws, Peer>`).
type peerConn struct {
	ws     *websocket.Conn
	peerID uint64
	userID string
	sendMu sync.Mutex
}

func (p *peerConn) send(ctx context.Context, frame []byte) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	return p.ws.Write(ctx, websocket.MessageBinary, frame)
}

func (p *peerConn) sendText(ctx context.Context, s string) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	return p.ws.Write(ctx, websocket.MessageText, []byte(s))
}

var errSessionClosed = errors.New("session closed")

// acquire returns a live session for docID, creating+locking one if needed.
// The caller MUST call release() exactly once.
//
// Lock order is always m.mu → s.mu (maybeClose takes them in that order
// too) — never the reverse.
func (m *sessionManager) acquire(ctx context.Context, docID string) (*docSession, error) {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return nil, errSessionClosed
	}
	if s, ok := m.sessions[docID]; ok {
		// refs/closing live under s.mu: without it, this refs++ raced
		// release()'s refs-- and a closing session could be handed out.
		s.mu.Lock()
		if !s.closing {
			s.refs++
			s.mu.Unlock()
			m.mu.Unlock()
			return s, nil
		}
		s.mu.Unlock()
	}
	m.mu.Unlock()

	// Slow path: advisory-lock + restore outside the manager mutex.
	lock, err := m.store.acquireLock(ctx, docID)
	if err != nil {
		return nil, err
	}
	snap, _, _, err := lock.loadSnapshot(ctx)
	if err != nil {
		lock.release(ctx)
		return nil, err
	}
	ops, err := lock.pendingOps(ctx)
	if err != nil {
		lock.release(ctx)
		return nil, err
	}
	engine, err := m.factory(ctx, snap, ops)
	if err != nil {
		lock.release(ctx)
		return nil, err
	}
	s := &docSession{
		id:      docID,
		mgr:     m,
		lock:    lock,
		engine:  engine,
		sockets: map[*peerConn]struct{}{},
		refs:    1,
	}
	s.onIdle = m.maybeClose

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		lock.release(ctx)
		engine.Close(ctx)
		return nil, errSessionClosed
	}
	if existing, ok := m.sessions[docID]; ok {
		// Possibly lost a race — another goroutine created the session first.
		existing.mu.Lock()
		live := !existing.closing
		if live {
			existing.refs++
		}
		existing.mu.Unlock()
		if live {
			lock.release(ctx)
			engine.Close(ctx)
			return existing, nil
		}
		// The old session is mid-close; the advisory lock it held has been
		// released (our acquireLock above would still be blocking otherwise),
		// so it is safe to replace the map entry.
	}
	m.sessions[docID] = s
	return s, nil
}

// release drops a reference; the session may close when nothing uses it.
func (m *sessionManager) release(s *docSession) {
	s.mu.Lock()
	s.refs--
	idle := s.refs <= 0 && len(s.sockets) == 0 && !s.closing
	cb := s.onIdle
	s.mu.Unlock()
	if idle && cb != nil {
		cb(s)
	}
}

// maybeClose closes and unregisters the session if it is still idle.
// The idle check (refs==0 && no sockets) and the map removal happen
// atomically under both locks — previously a socket attach or fresh
// acquire could slip between the removal and the close and be torn down
// under its feet.
func (m *sessionManager) maybeClose(s *docSession) {
	m.mu.Lock()
	s.mu.Lock()
	cur, ok := m.sessions[s.id]
	if !ok || cur != s || s.closing || s.refs > 0 || len(s.sockets) > 0 {
		s.mu.Unlock()
		m.mu.Unlock()
		return
	}
	s.closing = true
	delete(m.sessions, s.id)
	s.mu.Unlock()
	m.mu.Unlock()

	// Nobody can reach the session now: refs==0 and it is out of the map, so
	// flush/Close don't need s.mu (and must not take it — callers holding
	// s.mu would deadlock).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.flush(ctx); err != nil {
		m.log.Error("final flush failed", "doc", s.id, "err", err)
	}
	s.engine.Close(ctx)
	s.lock.release(ctx)
}

// closeAll flushes and closes every session (shutdown path).
func (m *sessionManager) closeAll() {
	m.mu.Lock()
	m.closing = true
	sessions := make([]*docSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		m.maybeClose(s)
	}
}

// flush persists the current snapshot + clears the op log — mirrors the Rust
// periodic flush / on-last-socket-close snapshot write.
func (s *docSession) flush(ctx context.Context) error {
	snap, err := s.engine.ExportSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("export snapshot: %w", err)
	}
	vvStr, _ := s.engine.VersionID(ctx)
	return s.lock.saveSnapshotTx(ctx, snap, []byte(vvStr), s.engine.KeepOpsOnFlush())
}

// attach registers a websocket peer and returns the peerConn.
func (s *docSession) attach(ws *websocket.Conn, userID string) *peerConn {
	p := &peerConn{ws: ws, userID: userID}
	s.mu.Lock()
	s.sockets[p] = struct{}{}
	s.mu.Unlock()
	return p
}

func (s *docSession) detach(p *peerConn) {
	s.mu.Lock()
	delete(s.sockets, p)
	s.mu.Unlock()
}

// broadcast sends a frame to every socket except exclude (Rust loop over
// sockets + error-tolerant unbounded_send).
func (s *docSession) broadcast(ctx context.Context, exclude *peerConn, frame []byte) {
	s.mu.Lock()
	peers := make([]*peerConn, 0, len(s.sockets))
	for p := range s.sockets {
		if p != exclude {
			peers = append(peers, p)
		}
	}
	s.mu.Unlock()
	for _, p := range peers {
		if err := p.send(ctx, frame); err != nil {
			s.mgr.log.Debug("broadcast send failed", "doc", s.id, "err", err)
		}
	}
}

// handlePeerMessage is the dispatch port of the Rust `on_message` match.
// Engine mutations run under s.mu (the wasm engine is not thread-safe), but
// all socket writes happen AFTER the mutex is released — a blocked ws write
// must never stall the session for every other peer.
func (s *docSession) handlePeerMessage(ctx context.Context, p *peerConn, m peerMsg) error {
	switch m.kind {
	case peerRegisterID:
		s.mu.Lock()
		p.peerID = m.peerID
		var err error
		if p.userID != "" {
			err = s.lock.upsertPeerUser(ctx, m.peerID, p.userID)
		}
		s.mu.Unlock()
		if err != nil {
			s.mgr.log.Warn("peer_user upsert failed", "doc", s.id, "err", err)
		}
		return nil

	case peerUpdate:
		if len(m.updates) == 0 {
			return nil
		}
		s.mu.Lock()
		err := s.engine.ImportBatch(ctx, m.updates)
		if err == nil {
			err = s.lock.appendOpsTx(ctx, m.updates)
		}
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("import/append updates: %w", err)
		}
		for _, u := range m.updates {
			s.broadcast(ctx, p, encRemoteUpdate(u))
		}
		if m.id != "" {
			return p.send(ctx, encRemoteUpdateAck(m.id))
		}
		return nil

	case peerAwareness:
		s.mu.Lock()
		err := s.engine.ApplyAwareness(ctx, m.awareness)
		var all []byte
		if err == nil {
			all, err = s.engine.EncodeAllAwareness(ctx)
		}
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("apply awareness: %w", err)
		}
		s.broadcast(ctx, p, encRemoteAwareness(all))
		return nil

	case peerRequestSince:
		s.mu.Lock()
		upd, err := s.engine.ExportUpdatesSince(ctx, m.vv)
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("export updates since: %w", err)
		}
		return p.send(ctx, encRemoteUpdateSince(upd, m.vv))

	case peerRequestSnapshot:
		s.mu.Lock()
		snap, err := s.engine.ExportSnapshot(ctx)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return p.send(ctx, encRemoteSnapshot(snap))

	default:
		return fmt.Errorf("unhandled peer message kind %d", m.kind)
	}
}

// initialSync builds the RemoteInitialSync frame for a fresh socket.
func (s *docSession) initialSync(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.engine.ExportShallowSnapshot(ctx)
	if err != nil {
		// Fall back to full snapshot (passthrough engine can't do shallow).
		snap, err = s.engine.ExportSnapshot(ctx)
		if err != nil {
			return nil, err
		}
	}
	aw, err := s.engine.EncodeAllAwareness(ctx)
	if err != nil {
		return nil, err
	}
	return encInitialSync(snap, aw), nil
}

// snapshotVV returns the encoded oplog version vector (for snapshot_vv
// persistence), or nil if the engine can't produce one.
func (s *docSession) snapshotVV(ctx context.Context) ([]byte, error) {
	vid, err := s.engine.VersionID(ctx)
	if err != nil {
		return nil, err
	}
	return []byte(vid), nil
}
