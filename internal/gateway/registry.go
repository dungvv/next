package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// registryBackend is the durable half of the Rust ConnectionManager
// (ConnectionRepo → DynamoDB). It tracks, across all gateway replicas:
//   - which connection ids belong to a user (presence / delivery receipts)
//   - which connections are watching an entity (track_entity)
//   - liveness via a heartbeat TTL (last_ping in the Rust model)
//
// Two backends exist: Valkey (primary) and Postgres (fallback when Valkey
// is down). A member is "live" while its connsess record exists — the TTL
// is refreshed by client pings and any inbound message, matching the Rust
// is_active_in_threshold(last_ping || created_at, 60s) semantics.
type registryBackend interface {
	name() string
	add(ctx context.Context, userID, connID string) error
	heartbeat(ctx context.Context, userID, connID string) error
	remove(ctx context.Context, userID, connID string) error
	connections(ctx context.Context, userID string) ([]string, error)
	entityUsers(ctx context.Context, entityType, entityID string) ([]string, error)
	trackEntity(ctx context.Context, connID, userID, entityType, entityID string) error
	untrackEntity(ctx context.Context, connID, entityType, entityID string) error
	pingEntity(ctx context.Context, connID, userID, entityType, entityID string) error
	sweep(ctx context.Context, ttl time.Duration) (int, error)
}

// Registry fronts the primary backend and falls back to Postgres when the
// primary errors. Reads fall back identically, so presence stays consistent
// during a Valkey outage.
type Registry struct {
	primary  registryBackend
	fallback registryBackend // may be nil
	ttl      time.Duration
}

func newRegistry(primary, fallback registryBackend, ttl time.Duration) *Registry {
	return &Registry{primary: primary, fallback: fallback, ttl: ttl}
}

// withBackend runs op against the primary, then the fallback on error.
func (r *Registry) withBackend(op func(b registryBackend) error) error {
	err := op(r.primary)
	if err == nil {
		return nil
	}
	if r.fallback == nil {
		return err
	}
	slog.Warn("gateway: primary registry op failed, falling back to postgres",
		"backend", r.primary.name(), "err", err)
	return op(r.fallback)
}

func (r *Registry) query(ctx context.Context, op func(b registryBackend) ([]string, error)) ([]string, error) {
	out, err := op(r.primary)
	if err == nil {
		return out, nil
	}
	if r.fallback == nil {
		return nil, err
	}
	slog.Warn("gateway: primary registry query failed, falling back to postgres",
		"backend", r.primary.name(), "err", err)
	return op(r.fallback)
}

func (r *Registry) Add(ctx context.Context, userID, connID string) error {
	return r.withBackend(func(b registryBackend) error { return b.add(ctx, userID, connID) })
}

func (r *Registry) Heartbeat(ctx context.Context, userID, connID string) error {
	return r.withBackend(func(b registryBackend) error { return b.heartbeat(ctx, userID, connID) })
}

func (r *Registry) Remove(ctx context.Context, userID, connID string) error {
	return r.withBackend(func(b registryBackend) error { return b.remove(ctx, userID, connID) })
}

// Connections returns the live connection ids for a user (any replica).
func (r *Registry) Connections(ctx context.Context, userID string) ([]string, error) {
	return r.query(ctx, func(b registryBackend) ([]string, error) {
		return b.connections(ctx, userID)
	})
}

// EntityUsers returns the deduped user ids with a live tracked connection
// on the entity — port of get_users_for_entity.
func (r *Registry) EntityUsers(ctx context.Context, entityType, entityID string) ([]string, error) {
	return r.query(ctx, func(b registryBackend) ([]string, error) {
		return b.entityUsers(ctx, entityType, entityID)
	})
}

// OnlineUsers filters userIDs to those with at least one live connection.
func (r *Registry) OnlineUsers(ctx context.Context, userIDs []string) ([]string, error) {
	online := make([]string, 0, len(userIDs))
	for _, u := range userIDs {
		conns, err := r.Connections(ctx, u)
		if err != nil {
			return nil, err
		}
		if len(conns) > 0 {
			online = append(online, u)
		}
	}
	return online, nil
}

func (r *Registry) TrackEntity(ctx context.Context, connID, userID, entityType, entityID string) error {
	return r.withBackend(func(b registryBackend) error {
		return b.trackEntity(ctx, connID, userID, entityType, entityID)
	})
}

func (r *Registry) UntrackEntity(ctx context.Context, connID, entityType, entityID string) error {
	return r.withBackend(func(b registryBackend) error {
		return b.untrackEntity(ctx, connID, entityType, entityID)
	})
}

func (r *Registry) PingEntity(ctx context.Context, connID, userID, entityType, entityID string) error {
	return r.withBackend(func(b registryBackend) error {
		return b.pingEntity(ctx, connID, userID, entityType, entityID)
	})
}

func (r *Registry) Sweep(ctx context.Context) (int, error) {
	var n int
	_, err := r.query(ctx, func(b registryBackend) ([]string, error) {
		var err error
		n, err = b.sweep(ctx, r.ttl)
		return nil, err
	})
	return n, err
}

// ---------------------------------------------------------------------------
// Valkey backend
// ---------------------------------------------------------------------------

// Key layout (replaces the DynamoDB PK/SK items):
//
//	conn:{userID}            SET    connID…            TTL=ConnTTL, heartbeat-refreshed
//	connsess:{connID}        STRING userID             TTL=ConnTTL — liveness marker
//	conne:{connID}           HASH   "type|id"→ms       TTL=ConnTTL — entities this conn tracks
//	entconns:{type}:{id}     SET    connID…            lazy-filtered by connsess, swept
//
// Members of conn:* and entconns:* are only "live" while their connsess key
// exists; the sweeper reaps members whose session expired.
const (
	keyUserConns = "conn:%s"
	keyConnSess  = "connsess:%s"
	keyConnEnts  = "conne:%s"
	keyEntConns  = "entconns:%s:%s"
)

func userConnsKey(u string) string    { return fmt.Sprintf(keyUserConns, u) }
func connSessKey(c string) string     { return fmt.Sprintf(keyConnSess, c) }
func connEntsKey(c string) string     { return fmt.Sprintf(keyConnEnts, c) }
func entConnsKey(t, id string) string { return fmt.Sprintf(keyEntConns, t, id) }
func entField(t, id string) string    { return t + "|" + id }
func splitEntField(f string) (string, string) {
	t, id, _ := strings.Cut(f, "|")
	return t, id
}

type valkeyRegistry struct {
	rdb *redis.Client
	ttl time.Duration
}

func newValkeyRegistry(rdb *redis.Client, ttl time.Duration) *valkeyRegistry {
	return &valkeyRegistry{rdb: rdb, ttl: ttl}
}

func (v *valkeyRegistry) name() string { return "valkey" }

func (v *valkeyRegistry) add(ctx context.Context, userID, connID string) error {
	_, err := v.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.SAdd(ctx, userConnsKey(userID), connID)
		p.Expire(ctx, userConnsKey(userID), v.ttl)
		p.Set(ctx, connSessKey(connID), userID, v.ttl)
		return nil
	})
	return err
}

// heartbeat refreshes the liveness TTLs. It re-asserts the records rather
// than only extending them so a missed/expired entry self-heals on the next
// ping — same effect as the Rust update_last_ping on an existing row.
func (v *valkeyRegistry) heartbeat(ctx context.Context, userID, connID string) error {
	_, err := v.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.SAdd(ctx, userConnsKey(userID), connID)
		p.Expire(ctx, userConnsKey(userID), v.ttl)
		p.Set(ctx, connSessKey(connID), userID, v.ttl)
		// Extend the entity-tracking hash if present; no-op when missing.
		p.ExpireXX(ctx, connEntsKey(connID), v.ttl)
		return nil
	})
	return err
}

func (v *valkeyRegistry) remove(ctx context.Context, userID, connID string) error {
	// Untrack every entity this conn was watching.
	fields, err := v.rdb.HKeys(ctx, connEntsKey(connID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	_, err = v.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, f := range fields {
			t, id := splitEntField(f)
			p.SRem(ctx, entConnsKey(t, id), connID)
		}
		p.SRem(ctx, userConnsKey(userID), connID)
		p.Del(ctx, connEntsKey(connID), connSessKey(connID))
		return nil
	})
	return err
}

// liveConns filters conn ids to those with a live connsess key.
func (v *valkeyRegistry) liveConns(ctx context.Context, connIDs []string) map[string]string {
	out := make(map[string]string, len(connIDs))
	if len(connIDs) == 0 {
		return out
	}
	keys := make([]string, len(connIDs))
	for i, id := range connIDs {
		keys[i] = connSessKey(id)
	}
	vals, err := v.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return out
	}
	for i, val := range vals {
		if s, ok := val.(string); ok && s != "" {
			out[connIDs[i]] = s
		}
	}
	return out
}

func (v *valkeyRegistry) connections(ctx context.Context, userID string) ([]string, error) {
	ids, err := v.rdb.SMembers(ctx, userConnsKey(userID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	live := v.liveConns(ctx, ids)
	out := make([]string, 0, len(live))
	for id := range live {
		out = append(out, id)
	}
	return out, nil
}

func (v *valkeyRegistry) entityUsers(ctx context.Context, entityType, entityID string) ([]string, error) {
	ids, err := v.rdb.SMembers(ctx, entConnsKey(entityType, entityID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	live := v.liveConns(ctx, ids)
	if len(live) == 0 {
		return nil, nil
	}
	// Per-entity liveness (Rust keeps last_ping on each entity item and
	// filters with is_active_in_threshold): a connection is only counted
	// while THIS entity's ping is fresh — a live connsess alone is not
	// enough, the entity's hash field must also be within the TTL window.
	connIDs := make([]string, 0, len(live))
	for id := range live {
		connIDs = append(connIDs, id)
	}
	pings := make([]*redis.StringCmd, 0, len(connIDs))
	if _, err := v.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, id := range connIDs {
			pings = append(pings, p.HGet(ctx, connEntsKey(id), entField(entityType, entityID)))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-v.ttl).UnixMilli()
	seen := make(map[string]struct{}, len(live))
	out := make([]string, 0, len(live))
	for i, connID := range connIDs {
		ts, err := pings[i].Int64()
		if err != nil || ts <= cutoff {
			continue // entity watch never recorded or stale
		}
		userID := live[connID]
		if _, dup := seen[userID]; !dup {
			seen[userID] = struct{}{}
			out = append(out, userID)
		}
	}
	return out, nil
}

func (v *valkeyRegistry) trackEntity(ctx context.Context, connID, userID, entityType, entityID string) error {
	now := time.Now().UnixMilli()
	_, err := v.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.SAdd(ctx, entConnsKey(entityType, entityID), connID)
		p.HSet(ctx, connEntsKey(connID), entField(entityType, entityID), now)
		p.Expire(ctx, connEntsKey(connID), v.ttl)
		// Ensure the session/membership records exist (e.g. after a restart).
		p.SAdd(ctx, userConnsKey(userID), connID)
		p.Expire(ctx, userConnsKey(userID), v.ttl)
		p.Set(ctx, connSessKey(connID), userID, v.ttl)
		return nil
	})
	return err
}

func (v *valkeyRegistry) untrackEntity(ctx context.Context, connID, entityType, entityID string) error {
	_, err := v.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.SRem(ctx, entConnsKey(entityType, entityID), connID)
		p.HDel(ctx, connEntsKey(connID), entField(entityType, entityID))
		return nil
	})
	return err
}

func (v *valkeyRegistry) pingEntity(ctx context.Context, connID, userID, entityType, entityID string) error {
	now := time.Now().UnixMilli()
	_, err := v.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		// Entity-level last_ping — the freshness check in entityUsers.
		p.HSet(ctx, connEntsKey(connID), entField(entityType, entityID), now)
		p.Expire(ctx, connEntsKey(connID), v.ttl)
		// Rust's update_last_entity_ping also propagates to the top-level
		// connection item; refresh the session/membership TTLs too.
		p.SAdd(ctx, userConnsKey(userID), connID)
		p.Expire(ctx, userConnsKey(userID), v.ttl)
		p.Set(ctx, connSessKey(connID), userID, v.ttl)
		return nil
	})
	return err
}

// sweep removes dead conn ids from conn:* and entconns:* sets — the port of
// scripts/stale_connections.rs. With Valkey the "stale" check is key expiry:
// any set member without a connsess key is reaped.
func (v *valkeyRegistry) sweep(ctx context.Context, _ time.Duration) (int, error) {
	removed := 0
	for _, pattern := range []string{"conn:*", "entconns:*"} {
		var cursor uint64
		for {
			keys, next, err := v.rdb.Scan(ctx, cursor, pattern, 200).Result()
			if err != nil {
				return removed, err
			}
			cursor = next
			for _, key := range keys {
				members, err := v.rdb.SMembers(ctx, key).Result()
				if err != nil {
					continue
				}
				if len(members) == 0 {
					continue
				}
				// Batch the liveness checks: MGET each member's connsess key.
				sessKeys := make([]string, len(members))
				for i, m := range members {
					sessKeys[i] = connSessKey(m)
				}
				vals, err := v.rdb.MGet(ctx, sessKeys...).Result()
				if err != nil {
					continue
				}
				dead := make([]string, 0, len(members))
				for i, m := range members {
					if vals[i] == nil {
						dead = append(dead, m)
					}
				}
				if len(dead) > 0 {
					if err := v.rdb.SRem(ctx, key, dead).Err(); err == nil {
						removed += len(dead)
					}
				}
			}
			if cursor == 0 {
				break
			}
		}
	}
	return removed, nil
}

// ---------------------------------------------------------------------------
// Postgres fallback backend (gateway_connections / gateway_entity_connections)
// ---------------------------------------------------------------------------

const registryDDL = `
CREATE TABLE IF NOT EXISTS gateway_connections (
    connection_id text PRIMARY KEY,
    user_id       text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_ping     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_connections_user_idx
    ON gateway_connections (user_id);

CREATE TABLE IF NOT EXISTS gateway_entity_connections (
    connection_id text NOT NULL,
    entity_type   text NOT NULL,
    entity_id     text NOT NULL,
    user_id       text NOT NULL,
    last_ping     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connection_id, entity_type, entity_id)
);
CREATE INDEX IF NOT EXISTS gateway_entity_connections_entity_idx
    ON gateway_entity_connections (entity_type, entity_id);
`

type pgRegistry struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

func newPGRegistry(pool *pgxpool.Pool, ttl time.Duration) (*pgRegistry, error) {
	if _, err := pool.Exec(context.Background(), registryDDL); err != nil {
		return nil, fmt.Errorf("ensure gateway registry tables: %w", err)
	}
	return &pgRegistry{pool: pool, ttl: ttl}, nil
}

func (p *pgRegistry) name() string { return "postgres" }

func (p *pgRegistry) cutoff() time.Time { return time.Now().Add(-p.ttl) }

func (p *pgRegistry) add(ctx context.Context, userID, connID string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO gateway_connections (connection_id, user_id, last_ping)
		VALUES ($1, $2, now())
		ON CONFLICT (connection_id) DO UPDATE SET last_ping = now(), user_id = $2`,
		connID, userID)
	return err
}

func (p *pgRegistry) heartbeat(ctx context.Context, userID, connID string) error {
	return p.add(ctx, userID, connID) // upsert refreshes last_ping
}

func (p *pgRegistry) remove(ctx context.Context, _ string, connID string) error {
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM gateway_entity_connections WHERE connection_id = $1`, connID); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx,
		`DELETE FROM gateway_connections WHERE connection_id = $1`, connID)
	return err
}

func (p *pgRegistry) connections(ctx context.Context, userID string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT connection_id FROM gateway_connections
		WHERE user_id = $1 AND last_ping > $2`, userID, p.cutoff())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (p *pgRegistry) entityUsers(ctx context.Context, entityType, entityID string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT e.user_id
		FROM gateway_entity_connections e
		JOIN gateway_connections c ON c.connection_id = e.connection_id
		WHERE e.entity_type = $1 AND e.entity_id = $2
		  AND c.last_ping > $3 AND e.last_ping > $3`,
		entityType, entityID, p.cutoff())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (p *pgRegistry) trackEntity(ctx context.Context, connID, userID, entityType, entityID string) error {
	if err := p.add(ctx, userID, connID); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO gateway_entity_connections
		    (connection_id, entity_type, entity_id, user_id, last_ping)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (connection_id, entity_type, entity_id)
		DO UPDATE SET last_ping = now(), user_id = $4`,
		connID, entityType, entityID, userID)
	return err
}

func (p *pgRegistry) untrackEntity(ctx context.Context, connID, entityType, entityID string) error {
	_, err := p.pool.Exec(ctx, `
		DELETE FROM gateway_entity_connections
		WHERE connection_id = $1 AND entity_type = $2 AND entity_id = $3`,
		connID, entityType, entityID)
	return err
}

func (p *pgRegistry) pingEntity(ctx context.Context, connID, _ /* userID */, entityType, entityID string) error {
	// Rust's update_last_entity_ping propagates to the top-level
	// connection item — refresh both timestamps.
	if _, err := p.pool.Exec(ctx, `
		UPDATE gateway_entity_connections SET last_ping = now()
		WHERE connection_id = $1 AND entity_type = $2 AND entity_id = $3`,
		connID, entityType, entityID); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `
		UPDATE gateway_connections SET last_ping = now()
		WHERE connection_id = $1`, connID)
	return err
}

func (p *pgRegistry) sweep(ctx context.Context, ttl time.Duration) (int, error) {
	cutoff := time.Now().Add(-ttl)
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM gateway_entity_connections WHERE last_ping < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n := int(tag.RowsAffected())
	tag, err = p.pool.Exec(ctx,
		`DELETE FROM gateway_connections WHERE last_ping < $1`, cutoff)
	if err != nil {
		return n, err
	}
	return n + int(tag.RowsAffected()), nil
}
