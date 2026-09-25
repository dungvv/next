package syncsvc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore is the Postgres replacement for the Rust service's D1 + DO-storage
// persistence layer:
//
//   - syncsvc_documents    — latest snapshot + its oplog version vector
//   - syncsvc_pending_ops  — op log applied on top of the snapshot
//   - syncsvc_peer_user    — port of D1 peer_user_map
//   - syncsvc_blame        — port of D1 blame
//
// Tables are created by ensureSchema (spike convenience — production should
// move these into crates/macro_db_client/migrations).
type pgStore struct {
	pool *pgxpool.Pool
}

func newPGStore(pool *pgxpool.Pool) *pgStore { return &pgStore{pool: pool} }

const syncSchema = `
CREATE TABLE IF NOT EXISTS syncsvc_documents (
    document_id TEXT PRIMARY KEY,
    snapshot    BYTEA NOT NULL,
    oplog_vv    BYTEA,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS syncsvc_pending_ops (
    document_id TEXT NOT NULL REFERENCES syncsvc_documents(document_id) ON DELETE CASCADE,
    seq         BIGSERIAL,
    op          BYTEA NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (document_id, seq)
);
CREATE TABLE IF NOT EXISTS syncsvc_peer_user (
    document_id TEXT NOT NULL,
    peer_id     BIGINT NOT NULL,
    user_id     TEXT NOT NULL,
    PRIMARY KEY (document_id, peer_id)
);
CREATE TABLE IF NOT EXISTS syncsvc_blame (
    document_id  TEXT NOT NULL,
    node_id      TEXT NOT NULL,
    peer_id      BIGINT NOT NULL,
    timestamp_ms BIGINT NOT NULL,
    PRIMARY KEY (document_id, node_id)
);`

func (s *pgStore) ensureSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, syncSchema)
	return err
}

// lockConn is a dedicated pool conn holding the per-document advisory lock.
type lockConn struct {
	conn    *pgxpool.Conn
	docID   string
	relOnce sync.Once // release() must be idempotent — double-release raced
}

// acquireLock blocks until pg_advisory_lock(hashtext(docID)) is held on a
// dedicated connection. The lock is released by release(), which runs
// pg_advisory_unlock and returns the conn to the pool. Pool acquisition
// ensures the same session-level lock semantics as the planned Go port.
func (s *pgStore) acquireLock(ctx context.Context, docID string) (*lockConn, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, docID); err != nil {
		conn.Release()
		return nil, fmt.Errorf("advisory lock %q: %w", docID, err)
	}
	return &lockConn{conn: conn, docID: docID}, nil
}

func (l *lockConn) release(ctx context.Context) {
	if l == nil {
		return
	}
	l.relOnce.Do(func() {
		if l.conn == nil {
			return
		}
		_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, l.docID)
		l.conn.Release()
		l.conn = nil
	})
}

// loadSnapshot returns (snapshot, oplogVV, exists).
func (l *lockConn) loadSnapshot(ctx context.Context) ([]byte, []byte, bool, error) {
	var snap, vv []byte
	err := l.conn.QueryRow(ctx,
		`SELECT snapshot, oplog_vv FROM syncsvc_documents WHERE document_id=$1`,
		l.docID).Scan(&snap, &vv)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	return snap, vv, true, nil
}

func (l *lockConn) pendingOps(ctx context.Context) ([][]byte, error) {
	rows, err := l.conn.Query(ctx,
		`SELECT op FROM syncsvc_pending_ops WHERE document_id=$1 ORDER BY seq`, l.docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ops [][]byte
	for rows.Next() {
		var op []byte
		if err := rows.Scan(&op); err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// appendOpsTx writes updates inside an explicit tx (Rust appends the op log
// transactionally with metadata). The document row must exist first —
// pending_ops.document_id is an FK — so the first op for a brand-new
// document (before any snapshot flush) would otherwise fail the insert.
// The placeholder uses an empty snapshot; replay treats it like a fresh doc.
func (l *lockConn) appendOpsTx(ctx context.Context, ops [][]byte) error {
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO syncsvc_documents (document_id, snapshot)
		VALUES ($1, '\x'::bytea)
		ON CONFLICT (document_id) DO NOTHING`, l.docID); err != nil {
		return err
	}
	for _, op := range ops {
		if _, err := tx.Exec(ctx,
			`INSERT INTO syncsvc_pending_ops (document_id, op) VALUES ($1,$2)`,
			l.docID, op); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// saveSnapshotTx mirrors the Rust flush: write snapshot+vv, clear applied ops,
// upsert the document row — all in one tx on the locked conn. keepOps preserves
// the pending-op log (used by the passthrough engine, which cannot merge ops
// into the snapshot it writes).
func (l *lockConn) saveSnapshotTx(ctx context.Context, snap, vv []byte, keepOps bool) error {
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO syncsvc_documents (document_id, snapshot, oplog_vv)
		VALUES ($1,$2,$3)
		ON CONFLICT (document_id)
		DO UPDATE SET snapshot=EXCLUDED.snapshot, oplog_vv=EXCLUDED.oplog_vv, updated_at=now()`,
		l.docID, snap, vv); err != nil {
		return err
	}
	if !keepOps {
		if _, err := tx.Exec(ctx,
			`DELETE FROM syncsvc_pending_ops WHERE document_id=$1`, l.docID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// exists reports whether the document row exists (unlocked read is fine).
func (s *pgStore) exists(ctx context.Context, docID string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM syncsvc_documents WHERE document_id=$1`, docID).Scan(&n)
	return n > 0, err
}

// --- D1 ports ---------------------------------------------------------------

func (l *lockConn) upsertPeerUser(ctx context.Context, peerID uint64, userID string) error {
	_, err := l.conn.Exec(ctx, `
		INSERT INTO syncsvc_peer_user (document_id, peer_id, user_id)
		VALUES ($1,$2,$3)
		ON CONFLICT (document_id, peer_id)
		DO UPDATE SET user_id=EXCLUDED.user_id`,
		l.docID, int64(peerID), userID)
	return err
}

func (l *lockConn) peerUser(ctx context.Context, peerID uint64) (string, bool, error) {
	var uid string
	err := l.conn.QueryRow(ctx,
		`SELECT user_id FROM syncsvc_peer_user WHERE document_id=$1 AND peer_id=$2`,
		l.docID, int64(peerID)).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return uid, err == nil, err
}

func (s *pgStore) listPeers(ctx context.Context, docID string) (map[uint64]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT peer_id, user_id FROM syncsvc_peer_user WHERE document_id=$1`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint64]string{}
	for rows.Next() {
		var pid int64
		var uid string
		if err := rows.Scan(&pid, &uid); err != nil {
			return nil, err
		}
		out[uint64(pid)] = uid
	}
	return out, rows.Err()
}

func (l *lockConn) upsertBlame(ctx context.Context, nodeID string, peerID uint64, tsMs int64) error {
	_, err := l.conn.Exec(ctx, `
		INSERT INTO syncsvc_blame (document_id, node_id, peer_id, timestamp_ms)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (document_id, node_id)
		DO UPDATE SET peer_id=EXCLUDED.peer_id, timestamp_ms=EXCLUDED.timestamp_ms`,
		l.docID, nodeID, int64(peerID), tsMs)
	return err
}

// blameInfo mirrors the D1 join: blame joined to peer_user_map for user_id.
func (s *pgStore) blameInfo(ctx context.Context, docID, nodeID string) (peerID uint64, userID string, ts int64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT b.peer_id, coalesce(p.user_id,''), b.timestamp_ms
		FROM syncsvc_blame b
		LEFT JOIN syncsvc_peer_user p
		  ON p.document_id=b.document_id AND p.peer_id=b.peer_id
		WHERE b.document_id=$1 AND b.node_id=$2`, docID, nodeID).
		Scan(&peerID, &userID, &ts)
	return
}
