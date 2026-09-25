package notification

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// This file ports the Rust StateMachineDriverA (crates/notification
// domain/models/email_notification_digest.rs): at ingress, decide per
// recipient whether the notification may later be batch-sent as a digest
// email.
//
// Decision tree (Rust ingest):
//
//	blocklisted type          → DontSend
//	no Macro account          → DontSend
//	push enabled              → Indeterminate (deferred to DriverB: batch iff
//	                            every push endpoint fails)
//	push disabled             → last_online > threshold → BatchSend now
//	                          → last_online ≤ threshold → DontSend
//
// In Rust "push enabled" = user is not muted AND has ≥1 device endpoint
// (any type). The deferred state only attaches to users with ≥1 iOS
// endpoint, because digest_state lives inside the APNS queue message.
//
// Errors propagate to the caller (ingress retries the message), matching
// Rust where driver errors abort send_notification.

// digestDecision is the per-recipient outcome of the ingress digest gate.
type digestDecision int

const (
	// digestDontSend — never batch this notification for this user.
	digestDontSend digestDecision = iota
	// digestDeferred — Indeterminate: batch on egress iff all pushes fail.
	digestDeferred
	// digestBatchNow — BatchWasQueued: the batcher already received it.
	digestBatchNow
)

// UserChecker reports which of the given email addresses have a Macro
// account (Rust UserExistenceChecker → SELECT id FROM "User" WHERE email).
type UserChecker interface {
	ExistingUsers(ctx context.Context, emails []string) (map[string]bool, error)
}

// LastOnlineChecker reports seconds-since-last-seen per user
// (Rust LastOnlineChecker). A user never tracked must report "offline
// forever" so they qualify for digest email.
type LastOnlineChecker interface {
	LastOnlineAges(ctx context.Context, userIDs []string) (map[string]time.Duration, error)
}

// pgUserChecker implements UserChecker against the macrodb "User" table.
type pgUserChecker struct {
	db dbQuerier
}

// dbQuerier is the subset of *pgxpool.Pool the checker needs (also satisfied
// by pgx.Tx in tests).
type dbQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (c pgUserChecker) ExistingUsers(ctx context.Context, emails []string) (map[string]bool, error) {
	rows, err := c.db.Query(ctx, `SELECT lower(email) FROM "User" WHERE lower(email) = ANY($1)`, emails)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out[e] = true
	}
	return out, rows.Err()
}

// redisLastOnline implements LastOnlineChecker over the same Redis keys the
// Rust last_online_tracker writes: `last_online:{user_id}` = RFC3339
// timestamp. A missing key means "never seen online" — reported as the max
// duration so the user qualifies for digests (Rust unwrap_or(Duration::MAX)).
type redisLastOnline struct {
	rdb *redis.Client
	now func() time.Time
}

func (l redisLastOnline) LastOnlineAges(ctx context.Context, userIDs []string) (map[string]time.Duration, error) {
	out := map[string]time.Duration{}
	if len(userIDs) == 0 {
		return out, nil
	}
	keys := make([]string, len(userIDs))
	for i, u := range userIDs {
		keys[i] = "last_online:" + u
	}
	vals, err := l.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	for i, v := range vals {
		age := time.Duration(math.MaxInt64) // never tracked → offline forever
		if s, ok := v.(string); ok && s != "" {
			if ts, perr := time.Parse(time.RFC3339, s); perr == nil {
				age = now.Sub(ts)
				if age < 0 {
					age = 0
				}
			}
		}
		out[userIDs[i]] = age
	}
	return out, nil
}

// digestGatePorts bundles the external lookups the gate needs.
type digestGatePorts struct {
	users     UserChecker
	online    LastOnlineChecker
	digests   DigestBatcher
	repo      Repository
	window    time.Duration // DriverA digest_window (Rust: 24h)
	threshold time.Duration // online recency threshold (Rust: 60min)
}

// ingest runs the DriverA decision for every row. Returns per-owner
// decisions; digestBatchNow decisions have already been queued on the
// batcher (mirroring DriverA::inner_store_batch).
func (g digestGatePorts) ingest(ctx context.Context, rows []Row) (map[string]digestDecision, error) {
	out := make(map[string]digestDecision, len(rows))
	if len(rows) == 0 || g.digests == nil {
		return out, nil
	}

	userIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		userIDs = append(userIDs, r.OwnerID)
	}

	// Batch the lookups the Rust machine performs per-user.
	muted, err := g.repo.GetMutedUsers(ctx, userIDs)
	if err != nil {
		return nil, fmt.Errorf("digest gate muted check: %w", err)
	}
	endpoints, err := g.repo.GetDeviceEndpoints(ctx, userIDs)
	if err != nil {
		return nil, fmt.Errorf("digest gate endpoints check: %w", err)
	}

	var exists map[string]bool
	if g.users != nil {
		emails := make([]string, len(userIDs))
		for i, u := range userIDs {
			emails[i] = emailPart(u)
		}
		exists, err = g.users.ExistingUsers(ctx, emails)
		if err != nil {
			return nil, fmt.Errorf("digest gate user existence check: %w", err)
		}
	}

	var ages map[string]time.Duration
	if g.online != nil {
		ages, err = g.online.LastOnlineAges(ctx, userIDs)
		if err != nil {
			return nil, fmt.Errorf("digest gate last-online check: %w", err)
		}
	}

	for _, row := range rows {
		out[row.OwnerID] = g.decide(row, muted, endpoints, exists, ages)
		if out[row.OwnerID] == digestBatchNow {
			if err := g.digests.Add(ctx, row, g.window); err != nil {
				return nil, fmt.Errorf("digest gate store batch: %w", err)
			}
		}
	}
	return out, nil
}

func (g digestGatePorts) decide(row Row, muted map[string]bool,
	endpoints map[string][]DeviceEndpoint, exists map[string]bool,
	ages map[string]time.Duration) digestDecision {

	// 1. blocklist (new_email, invites) — never digest.
	if digestEmailBlockList[row.NotificationEventType] {
		return digestDontSend
	}
	// 2. user must have a Macro account. A nil checker means "cannot check"
	// — treated as existing so digests still work without a macrodb pool.
	if g.users != nil && !exists[emailPart(row.OwnerID)] {
		return digestDontSend
	}
	// 3. push enabled = not muted AND at least one registered endpoint.
	eps := endpoints[row.OwnerID]
	pushEnabled := !muted[row.OwnerID] && len(eps) > 0
	if pushEnabled {
		// Indeterminate — but the deferred state can only ride on the iOS
		// push message, which exists only for users with ≥1 ios endpoint.
		for _, ep := range eps {
			if ep.Type == DeviceIOS {
				return digestDeferred
			}
		}
		return digestDontSend
	}
	// 4. push disabled — batch only when the user has been offline longer
	// than the threshold (Rust: age > threshold → BatchSend).
	if g.online == nil || ages[row.OwnerID] > g.threshold {
		return digestBatchNow
	}
	return digestDontSend
}
