package mcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// In-flight handshake state TTLs, mirroring domain::service constants.
const (
	// PendingAuthTTL bounds a /authorize → /oauth/callback window.
	PendingAuthTTL = 10 * time.Minute
	// AuthorizationCodeTTL bounds a callback → /token redemption window.
	AuthorizationCodeTTL = 5 * time.Minute
)

// InflightAuthStore persists short-lived OAuth handshake state (the
// Valkey/Redis backend replaces outbound::redis; the in-memory backend keeps
// single-binary deployments working without Valkey).
type InflightAuthStore interface {
	InsertPending(ctx context.Context, sessionID string, pending PendingAuthorization) error
	TakePending(ctx context.Context, sessionID string) (*PendingAuthorization, error)
	InsertIssued(ctx context.Context, code string, issued IssuedAuthorizationCode) error
	TakeIssued(ctx context.Context, code string) (*IssuedAuthorizationCode, error)
	// CleanupExpired removes expired entries for stores that do not enforce
	// TTLs themselves (no-op for Redis).
	CleanupExpired(ctx context.Context) error
}

// ---------------------------------------------------------------------------
// Redis/Valkey backend (outbound/redis.rs)
// ---------------------------------------------------------------------------

const (
	pendingKeyPrefix = "mcp_auth_proxy:pending:"
	issuedKeyPrefix  = "mcp_auth_proxy:issued:"
)

// RedisInflightAuth stores in-flight state in Valkey/Redis with TTLs.
type RedisInflightAuth struct {
	Client redis.UniversalClient
}

func (RedisInflightAuth) pendingKey(sessionID string) string { return pendingKeyPrefix + sessionID }
func (RedisInflightAuth) issuedKey(code string) string       { return issuedKeyPrefix + code }

func (s RedisInflightAuth) InsertPending(ctx context.Context, sessionID string, pending PendingAuthorization) error {
	raw, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return s.Client.SetEx(ctx, s.pendingKey(sessionID), raw, PendingAuthTTL).Err()
}

func (s RedisInflightAuth) TakePending(ctx context.Context, sessionID string) (*PendingAuthorization, error) {
	raw, err := s.Client.GetDel(ctx, s.pendingKey(sessionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p PendingAuthorization
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s RedisInflightAuth) InsertIssued(ctx context.Context, code string, issued IssuedAuthorizationCode) error {
	raw, err := json.Marshal(issued)
	if err != nil {
		return err
	}
	return s.Client.SetEx(ctx, s.issuedKey(code), raw, AuthorizationCodeTTL).Err()
}

func (s RedisInflightAuth) TakeIssued(ctx context.Context, code string) (*IssuedAuthorizationCode, error) {
	raw, err := s.Client.GetDel(ctx, s.issuedKey(code)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var i IssuedAuthorizationCode
	if err := json.Unmarshal(raw, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// CleanupExpired is a no-op: Redis enforces TTL itself.
func (RedisInflightAuth) CleanupExpired(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// In-memory backend — single-binary fallback
// ---------------------------------------------------------------------------

type memEntry struct {
	value     any
	expiresAt time.Time
}

// MemoryInflightAuth is a process-local InflightAuthStore for single-binary
// deployments without Valkey. Entries expire lazily on read and eagerly via
// CleanupExpired.
type MemoryInflightAuth struct {
	mu      sync.Mutex
	pending map[string]memEntry
	issued  map[string]memEntry
}

// NewMemoryInflightAuth builds an empty in-memory store.
func NewMemoryInflightAuth() *MemoryInflightAuth {
	return &MemoryInflightAuth{
		pending: map[string]memEntry{},
		issued:  map[string]memEntry{},
	}
}

func (m *MemoryInflightAuth) InsertPending(_ context.Context, sessionID string, pending PendingAuthorization) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[sessionID] = memEntry{value: pending, expiresAt: time.Now().Add(PendingAuthTTL)}
	return nil
}

func (m *MemoryInflightAuth) TakePending(_ context.Context, sessionID string) (*PendingAuthorization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.pending[sessionID]
	if !ok || time.Now().After(e.expiresAt) {
		delete(m.pending, sessionID)
		return nil, nil
	}
	delete(m.pending, sessionID)
	p := e.value.(PendingAuthorization)
	return &p, nil
}

func (m *MemoryInflightAuth) InsertIssued(_ context.Context, code string, issued IssuedAuthorizationCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issued[code] = memEntry{value: issued, expiresAt: time.Now().Add(AuthorizationCodeTTL)}
	return nil
}

func (m *MemoryInflightAuth) TakeIssued(_ context.Context, code string) (*IssuedAuthorizationCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.issued[code]
	if !ok || time.Now().After(e.expiresAt) {
		delete(m.issued, code)
		return nil, nil
	}
	delete(m.issued, code)
	i := e.value.(IssuedAuthorizationCode)
	return &i, nil
}

// CleanupExpired sweeps expired entries from both maps.
func (m *MemoryInflightAuth) CleanupExpired(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, e := range m.pending {
		if now.After(e.expiresAt) {
			delete(m.pending, k)
		}
	}
	for k, e := range m.issued {
		if now.After(e.expiresAt) {
			delete(m.issued, k)
		}
	}
	return nil
}
