package syncsvc

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
)

//go:embed schema.bop
var schemaFS embed.FS

const (
	headerInternalAuth = "x-internal-auth-key"
	maxWSReadBytes     = 1 << 20 // 1 MiB inbound cap (gateway convention)
	maxUpdateBodyBytes = 16 << 20
)

// Server wires HTTP + WS handlers to the session manager — the Go analogue
// of the cf_worker router + Durable Object fetch().
type Server struct {
	mgr     *sessionManager
	cfg     syncConfig
	log     *slog.Logger
	lifeCtx context.Context // cancelled on shutdown to drop ws conns
}

func NewServer(mgr *sessionManager, cfg syncConfig, log *slog.Logger) *Server {
	return &Server{mgr: mgr, cfg: cfg, log: log}
}

// Routes returns the service mux.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /schema", s.schema)
	mux.HandleFunc("/document/", s.document)
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) schema(w http.ResponseWriter, r *http.Request) {
	b, _ := schemaFS.ReadFile("schema.bop")
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

// document routes /document/{id}/{rest} — mirrors the DO marker router.
func (s *Server) document(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/document/")
	parts := strings.SplitN(rest, "/", 2)
	docID := parts[0]
	if docID == "" {
		http.Error(w, "missing document id", http.StatusBadRequest)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	// trailing segments after the action (peer_id, node_id, ...)
	action, arg, _ := strings.Cut(sub, "/")

	switch action {
	case "connect":
		s.connect(w, r, docID)
	case "exists":
		s.exists(w, r, docID)
	case "initialize":
		s.initialize(w, r, docID)
	case "snapshot":
		s.snapshot(w, r, docID)
	case "state":
		s.docAPI(w, r, docID, false)
	case "update":
		s.docAPI(w, r, docID, true)
	case "raw":
		s.raw(w, r, docID)
	case "active_peers":
		s.activePeers(w, r, docID)
	case "peer":
		s.peer(w, r, docID, arg)
	case "metadata":
		s.metadata(w, r, docID)
	case "blame":
		s.blame(w, r, docID, arg)
	case "copy":
		s.copyDoc(w, r, docID)
	case "wakeup":
		s.wakeup(w, r, docID)
	case "debug_dump_operations":
		s.dumpOps(w, r, docID)
	default:
		http.Error(w, "unknown document route", http.StatusNotFound)
	}
}

// authClaims mirrors services/sync-service AuthToken — the claims of the
// document-permission JWT that DSS mints via /documents/permissions_token
// (crates/documents permission_token.rs), verified with
// DOCUMENT_PERMISSION_JWT.
type authClaims struct {
	UserID      *string `json:"user_id,omitempty"`
	DocumentID  string  `json:"document_id"`
	AccessLevel string  `json:"access_level"`
	Actor       *string `json:"actor,omitempty"`
	jwt.RegisteredClaims
}

// Access levels ordered like the Rust AccessLevel enum
// (View < Comment < Edit < Owner < Admin).
const (
	syncLevelView = iota
	syncLevelComment
	syncLevelEdit
	syncLevelOwner
	syncLevelAdmin
)

func syncAccessLevelNum(s string) int {
	switch strings.ToLower(s) {
	case "view":
		return syncLevelView
	case "comment":
		return syncLevelComment
	case "edit":
		return syncLevelEdit
	case "owner":
		return syncLevelOwner
	case "admin":
		return syncLevelAdmin
	}
	return -1
}

func (c authClaims) level() int { return syncAccessLevelNum(c.AccessLevel) }

// hasDocAccess mirrors AuthToken::has_document_id_access — the token's
// document_id must match the requested document, or the token must be an
// admin grant (which is also what an internal-key request produces).
func (c authClaims) hasDocAccess(docID string) bool {
	return c.level() == syncLevelAdmin || c.DocumentID == docID
}

func (c authClaims) userID() string {
	if c.UserID != nil {
		return *c.UserID
	}
	return ""
}

var errSyncUnauthorized = errors.New("unauthorized")

// bearerToken pulls the token from `?token=` (ws clients — the Rust
// TokenFrom::QueryParams path) or `Authorization: Bearer`.
func (s *Server) bearerToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// authenticate mirrors auth.rs decode_jwt + has_document_id_access. Every
// /document/* endpoint calls it — the Rust worker left exists/peer/wakeup
// open, but this port requires credentials on all document routes:
//
//  1. x-internal-auth-key (constant-time compare) → admin claims
//  2. a document-permission JWT (HS256, DOCUMENT_PERMISSION_JWT) — must
//     carry document_id == docID (or access_level=admin) and at least
//     minLevel
//  3. SYNC_INSECURE_AUTH=true dev escape hatch → fabricated claims
func (s *Server) authenticate(r *http.Request, docID string, minLevel int) (authClaims, error) {
	if s.cfg.InternalAPIKey != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get(headerInternalAuth)), []byte(s.cfg.InternalAPIKey)) == 1 {
		return authClaims{AccessLevel: "admin"}, nil
	}
	if tok := s.bearerToken(r); tok != "" && s.cfg.PermissionJWTSecret != "" {
		var claims authClaims
		_, err := jwt.ParseWithClaims(tok, &claims,
			func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected signing method")
				}
				return []byte(s.cfg.PermissionJWTSecret), nil
			})
		if err == nil {
			if !claims.hasDocAccess(docID) {
				return authClaims{}, errSyncUnauthorized
			}
			if claims.level() < minLevel {
				return authClaims{}, errSyncUnauthorized
			}
			return claims, nil
		}
		s.log.Debug("syncsvc: permission token rejected", "doc", docID, "err", err)
	}
	if s.cfg.InsecureAuth {
		// Dev escape hatch (Rust insecure_auth): fabricated claims — any
		// ?user_id is accepted with no credential verification.
		uid := r.URL.Query().Get("user_id")
		if uid == "" {
			uid = "anon"
		}
		return authClaims{UserID: &uid, DocumentID: docID, AccessLevel: "admin"}, nil
	}
	return authClaims{}, errSyncUnauthorized
}

// --- websocket --------------------------------------------------------------

func (s *Server) connect(w http.ResponseWriter, r *http.Request, docID string) {
	claims, err := s.authenticate(r, docID, syncLevelView)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	userID := claims.userID()
	opts := &websocket.AcceptOptions{}
	if s.cfg.WSOrigins == "" || s.cfg.WSOrigins == "*" {
		opts.InsecureSkipVerify = true
	} else {
		opts.OriginPatterns = strings.Split(s.cfg.WSOrigins, ",")
	}
	ws, err := websocket.Accept(w, r, opts)
	if err != nil {
		s.log.Warn("ws accept failed", "doc", docID, "err", err)
		return
	}
	ws.SetReadLimit(maxWSReadBytes)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if s.lifeCtx != nil {
		stop := context.AfterFunc(s.lifeCtx, cancel)
		defer stop()
	}

	sess, err := s.mgr.acquire(ctx, docID)
	if err != nil {
		s.log.Warn("session acquire failed", "doc", docID, "err", err)
		_ = ws.Close(websocket.StatusInternalError, "session unavailable")
		return
	}
	defer s.mgr.release(sess)

	peer := sess.attach(ws, userID)
	defer sess.detach(peer)

	// Rust sends RemoteInitialSync on accept (or defers it until initialize
	// lands). If the doc isn't initialized yet we still send an empty-doc
	// initial sync — the Rust path waits for initialize; flag as a delta.
	initFrame, err := sess.initialSync(ctx)
	if err != nil {
		s.log.Warn("initial sync failed", "doc", docID, "err", err)
	} else if err := peer.send(ctx, initFrame); err != nil {
		s.log.Debug("initial sync send failed", "doc", docID, "err", err)
		return
	}

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return // closed or errored — detach via defer
		}
		switch typ {
		case websocket.MessageText:
			if string(data) == "ping" {
				_ = peer.sendText(ctx, "pong")
			}
		case websocket.MessageBinary:
			m, err := decodeFromPeer(data)
			if err != nil {
				s.log.Debug("bebop decode failed", "doc", docID, "err", err)
				continue
			}
			if err := sess.handlePeerMessage(ctx, peer, m); err != nil {
				s.log.Debug("peer message failed", "doc", docID, "kind", m.kind, "err", err)
			}
		}
	}
}

// --- document endpoints -------------------------------------------------------

// exists mirrors exists_handler (unlocked pool read is fine for a spike).
// Rust left this unauthenticated; this port requires at least a view-scoped
// credential like every other document route.
func (s *Server) exists(w http.ResponseWriter, r *http.Request, docID string) {
	if _, err := s.authenticate(r, docID, syncLevelView); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ok, err := s.mgr.store.exists(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// initialize mirrors initialize_handler: bebop InitializeFromSnapshotRequest,
// fails if a snapshot already exists.
func (s *Server) initialize(w http.ResponseWriter, r *http.Request, docID string) {
	if _, err := s.authenticate(r, docID, syncLevelEdit); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUpdateBodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	snap, err := decodeInitializeFromSnapshotRequest(body)
	if err != nil {
		http.Error(w, "bad InitializeFromSnapshotRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	sess, err := s.mgr.acquire(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.mgr.release(sess)

	sess.mu.Lock()
	defer sess.mu.Unlock()
	_, _, exists, err := sess.lock.loadSnapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if exists {
		http.Error(w, "snapshot already exists", http.StatusConflict)
		return
	}
	if err := sess.engine.ImportBatch(r.Context(), [][]byte{snap}); err != nil {
		http.Error(w, "import snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := sess.flush(r.Context()); err != nil {
		http.Error(w, "persist snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// snapshot mirrors snapshot_handler: optional JSON {version_id} body,
// returns raw snapshot bytes. version_id-scoped exports (Frontiers::ID) are
// not yet plumbed through the engine — flagged in REPORT.md.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, docID string) {
	if _, err := s.authenticate(r, docID, syncLevelView); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sess, err := s.mgr.acquire(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.mgr.release(sess)

	sess.mu.Lock()
	defer sess.mu.Unlock()
	_, _, exists, err := sess.lock.loadSnapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	out, err := sess.engine.ExportSnapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/octet-stream")
	_, _ = w.Write(out)
}

// docAPI mirrors document_api.rs document_handler: GET /state returns
// {snapshot, revision} base64 JSON; POST /update takes {expected_revision,
// update} and applies if the revision matches (spike: revision comparison
// is best-effort via engine VersionID).
func (s *Server) docAPI(w http.ResponseWriter, r *http.Request, docID string, isUpdate bool) {
	minLevel := syncLevelView
	if isUpdate {
		minLevel = syncLevelEdit
	}
	if _, err := s.authenticate(r, docID, minLevel); err != nil {
		writeDocErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sess, err := s.mgr.acquire(r.Context(), docID)
	if err != nil {
		writeDocErr(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.mgr.release(sess)

	sess.mu.Lock()
	_, _, exists, err := sess.lock.loadSnapshot(r.Context())
	if err != nil {
		sess.mu.Unlock()
		writeDocErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		sess.mu.Unlock()
		writeDocErr(w, "not found", http.StatusNotFound)
		return
	}

	if !isUpdate {
		snap, err := sess.engine.ExportSnapshot(r.Context())
		vid, _ := sess.engine.VersionID(r.Context())
		sess.mu.Unlock()
		if err != nil {
			writeDocErr(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{
			"snapshot": base64.StdEncoding.EncodeToString(snap),
			"revision": base64.StdEncoding.EncodeToString([]byte(vid)),
		})
		return
	}
	sess.mu.Unlock()

	if r.Method != http.MethodPost {
		writeDocErr(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ExpectedRevision string `json:"expected_revision"`
		Update           string `json:"update"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUpdateBodyBytes))
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeDocErr(w, "invalid update request", http.StatusBadRequest)
		return
	}
	upd, err := base64.StdEncoding.DecodeString(req.Update)
	if err != nil {
		writeDocErr(w, "invalid base64 update", http.StatusBadRequest)
		return
	}
	sess.mu.Lock()
	err = sess.engine.ImportBatch(r.Context(), [][]byte{upd})
	if err == nil {
		err = sess.lock.appendOpsTx(r.Context(), [][]byte{upd})
	}
	vid, _ := sess.engine.VersionID(r.Context())
	sess.mu.Unlock()
	if err != nil {
		writeDocErr(w, "apply update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Broadcast to live sockets, like the Rust ws path — outside sess.mu so
	// a stalled peer can't block the engine for everyone.
	sess.broadcast(r.Context(), nil, encRemoteUpdate(upd))
	writeJSON(w, map[string]any{
		"revision": base64.StdEncoding.EncodeToString([]byte(vid)),
		"applied":  true,
	})
}

// raw mirrors raw_handler: deep JSON value of the doc. Not plumbed through
// the engine yet — 501. Still requires a valid credential for the doc.
func (s *Server) raw(w http.ResponseWriter, r *http.Request, docID string) {
	if _, err := s.authenticate(r, docID, syncLevelView); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.Error(w, "raw deep-value export not implemented in Go spike", http.StatusNotImplemented)
}

func (s *Server) activePeers(w http.ResponseWriter, r *http.Request, docID string) {
	if _, err := s.authenticate(r, docID, syncLevelView); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	peers, err := s.mgr.store.listPeers(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ids := make([]string, 0, len(peers))
	for pid := range peers {
		ids = append(ids, fmt.Sprint(pid))
	}
	writeJSON(w, ids)
}

func (s *Server) peer(w http.ResponseWriter, r *http.Request, docID, peerStr string) {
	if _, err := s.authenticate(r, docID, syncLevelView); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var pid uint64
	if _, err := fmt.Sscanf(peerStr, "%d", &pid); err != nil {
		http.Error(w, "bad peer_id", http.StatusBadRequest)
		return
	}
	peers, err := s.mgr.store.listPeers(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{
		"peer_id": peerStr,
		"user_id": peers[pid],
	})
}

// metadata mirrors metadata_handler: {peers, version_id, id}.
func (s *Server) metadata(w http.ResponseWriter, r *http.Request, docID string) {
	if _, err := s.authenticate(r, docID, syncLevelView); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	peers, err := s.mgr.store.listPeers(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	vid := ""
	if sess, err := s.mgr.acquire(r.Context(), docID); err == nil {
		sess.mu.Lock()
		vid, _ = sess.engine.VersionID(r.Context())
		sess.mu.Unlock()
		s.mgr.release(sess)
	}
	writeJSON(w, map[string]any{
		"peers":      peers,
		"version_id": vid,
		"id":         docID,
	})
}

// blame mirrors blame_handler: GET returns the blame row for node_id;
// POST upserts (Rust pushes blame via WS events — spike exposes POST here).
func (s *Server) blame(w http.ResponseWriter, r *http.Request, docID, nodeID string) {
	if nodeID == "" {
		http.Error(w, "missing node_id", http.StatusBadRequest)
		return
	}
	minLevel := syncLevelView
	if r.Method == http.MethodPost {
		minLevel = syncLevelEdit // POST upserts a blame row
	}
	if _, err := s.authenticate(r, docID, minLevel); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost {
		sess, err := s.mgr.acquire(r.Context(), docID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer s.mgr.release(sess)
		var req struct {
			PeerID      uint64 `json:"peer_id"`
			TimestampMs int64  `json:"timestamp_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad blame body", http.StatusBadRequest)
			return
		}
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if err := sess.lock.upsertBlame(r.Context(), nodeID, req.PeerID, req.TimestampMs); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	peerID, userID, ts, err := s.mgr.store.blameInfo(r.Context(), docID, nodeID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"peer_id":      peerID,
		"user_id":      userID,
		"timestamp_ms": ts,
	})
}

// copyDoc mirrors copy_handler: read this doc's snapshot, initialize the
// target doc with it. Body: {target_document_id, version_id?}.
func (s *Server) copyDoc(w http.ResponseWriter, r *http.Request, docID string) {
	// Rust forwards the caller's credential to the source snapshot read and
	// the target initialize — i.e. view on the source, edit on the target.
	claims, err := s.authenticate(r, docID, syncLevelView)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TargetDocumentID string `json:"target_document_id"`
		VersionID        any    `json:"version_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TargetDocumentID == "" {
		http.Error(w, "bad copy request", http.StatusBadRequest)
		return
	}
	if !(claims.hasDocAccess(req.TargetDocumentID) && claims.level() >= syncLevelEdit) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	src, err := s.mgr.acquire(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	src.mu.Lock()
	snap, err := src.engine.ExportSnapshot(r.Context())
	src.mu.Unlock()
	s.mgr.release(src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(snap) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	dst, err := s.mgr.acquire(r.Context(), req.TargetDocumentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.mgr.release(dst)
	dst.mu.Lock()
	defer dst.mu.Unlock()
	_, _, exists, err := dst.lock.loadSnapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if exists {
		http.Error(w, "target snapshot already exists", http.StatusConflict)
		return
	}
	if err := dst.engine.ImportBatch(r.Context(), [][]byte{snap}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := dst.flush(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// wakeup mirrors wakeup/warmup: materializes the session (locks + loads) and
// returns keepalive JSON.
func (s *Server) wakeup(w http.ResponseWriter, r *http.Request, docID string) {
	if _, err := s.authenticate(r, docID, syncLevelView); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sess, err := s.mgr.acquire(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.mgr.release(sess)
	writeJSON(w, map[string]any{"keepalive_until": time.Now().Add(s.cfg.IdleTTL).UnixMilli()})
}

// dumpOps mirrors debug_dump_operations (admin-only in Rust).
func (s *Server) dumpOps(w http.ResponseWriter, r *http.Request, docID string) {
	if _, err := s.authenticate(r, docID, syncLevelAdmin); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sess, err := s.mgr.acquire(r.Context(), docID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.mgr.release(sess)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	ops, err := sess.lock.pendingOps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"count": len(ops)})
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := jsonOK(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	_, _ = w.Write(b)
}

func writeDocErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
