package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/macro-inc/macro/pkg/mail"
)

// DigestBatch is a claimed per-user batch of notifications.
type DigestBatch struct {
	UserID        string
	Notifications []Row
}

// ClaimResultKind distinguishes claim_ready_digest outcomes.
type ClaimResultKind int

const (
	// ClaimReady means a batch was claimed.
	ClaimReady ClaimResultKind = iota
	// ClaimEmpty means nothing is pending.
	ClaimEmpty
	// ClaimWait means digests exist but none are due yet.
	ClaimWait
)

// ClaimResult mirrors Rust ports::ClaimResult.
type ClaimResult struct {
	Kind  ClaimResultKind
	Batch *DigestBatch
	Wait  time.Duration
}

// DigestBatcher ports the Rust DigestBatcher port: collect notifications
// per user and flush them as a digest email after a delay.
type DigestBatcher interface {
	// Add appends a notification to the user's pending digest, scheduling the
	// send send_after from now (NX — first notification sets the deadline).
	Add(ctx context.Context, row Row, sendAfter time.Duration) error
	// ClaimReady atomically claims one due digest.
	ClaimReady(ctx context.Context) (ClaimResult, error)
}

// RedisDigestBatcher ports outbound/digest_batcher.rs:
//
//	digest:{user_id}          — list of serialized rows
//	digest_processing:{user}  — snapshot claimed via RENAME
//	digest_pending_users      — zset, score = send_at unix
type RedisDigestBatcher struct {
	rdb *redis.Client
}

// NewRedisDigestBatcher builds the batcher on a go-redis client.
func NewRedisDigestBatcher(rdb *redis.Client) *RedisDigestBatcher {
	return &RedisDigestBatcher{rdb: rdb}
}

func (b *RedisDigestBatcher) Add(ctx context.Context, row Row, sendAfter time.Duration) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	digestKey := "digest:" + row.OwnerID
	if err := b.rdb.RPush(ctx, digestKey, raw).Err(); err != nil {
		return err
	}
	sendAt := float64(time.Now().Add(sendAfter).Unix())
	return b.rdb.ZAddArgs(ctx, "digest_pending_users", redis.ZAddArgs{
		NX:      true,
		Members: []redis.Z{{Score: sendAt, Member: row.OwnerID}},
	}).Err()
}

func (b *RedisDigestBatcher) ClaimReady(ctx context.Context) (ClaimResult, error) {
	now := time.Now().Unix()
	popped, err := b.rdb.ZPopMin(ctx, "digest_pending_users", 1).Result()
	if err != nil {
		return ClaimResult{}, err
	}
	if len(popped) == 0 {
		return ClaimResult{Kind: ClaimEmpty}, nil
	}
	userID, _ := popped[0].Member.(string)
	score := popped[0].Score

	if score > float64(now) {
		// Not due yet — put it back and report the wait.
		_ = b.rdb.ZAdd(ctx, "digest_pending_users", redis.Z{Score: score, Member: userID}).Err()
		return ClaimResult{Kind: ClaimWait, Wait: time.Duration(int64(score)-now) * time.Second}, nil
	}

	if age := now - int64(score); age > int64(digestStalenessThreshold/time.Second) {
		_ = b.rdb.Del(ctx, "digest:"+userID).Err()
		slog.Warn("discarding stale email digest", "user", userID, "age_hours", age/3600)
		return ClaimResult{Kind: ClaimEmpty}, nil
	}

	digestKey := "digest:" + userID
	processingKey := "digest_processing:" + userID
	requeue := func() {
		// Best-effort restoration: put the user back on the pending zset and
		// move the processing snapshot back to the live key so the batch is
		// claimed again instead of stranded.
		_ = b.rdb.Rename(ctx, processingKey, digestKey).Err()
		_ = b.rdb.ZAdd(ctx, "digest_pending_users", redis.Z{Score: score, Member: userID}).Err()
	}
	if err := b.rdb.Rename(ctx, digestKey, processingKey).Err(); err != nil {
		if !isNoSuchKey(err) {
			// Transient Redis failure — the user was already popped from the
			// pending set; requeue so the digest is retried, not lost.
			requeue()
			return ClaimResult{}, err
		}
		// digest:{user} missing. If a previous claim crashed between RENAME
		// and DEL, the batch may still sit under the processing key — adopt
		// it rather than dropping it.
		if n, eerr := b.rdb.Exists(ctx, processingKey).Result(); eerr != nil || n == 0 {
			return ClaimResult{Kind: ClaimEmpty}, nil
		}
	}
	items, err := b.rdb.LRange(ctx, processingKey, 0, -1).Result()
	if err != nil {
		// Read failed after the snapshot — restore the claim so the batch is
		// re-claimed later rather than silently dropped.
		requeue()
		return ClaimResult{}, err
	}
	_ = b.rdb.Del(ctx, processingKey).Err()
	var rows []Row
	for _, item := range items {
		var r Row
		if err := json.Unmarshal([]byte(item), &r); err != nil {
			slog.Error("failed to deserialize digest notification", "err", err)
			continue
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return ClaimResult{Kind: ClaimEmpty}, nil
	}
	return ClaimResult{Kind: ClaimReady, Batch: &DigestBatch{UserID: userID, Notifications: rows}}, nil
}

// isNoSuchKey reports whether a Redis error is the "no such key" reply
// produced by RENAME on a missing source.
func isNoSuchKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such key")
}

// digestStalenessThreshold is set from Config at startup.
var digestStalenessThreshold = 48 * time.Hour

// digestEmailBlockList ports model_notifications::digest_state::digest_email_block_list:
// notification types that never trigger digest emails.
var digestEmailBlockList = map[string]bool{
	"new_email":       true,
	"invite_to_team":  true,
	"invite_to_macro": true,
}

// DigestFlusher polls the batcher and sends digest emails via MailPort.
type DigestFlusher struct {
	batcher      DigestBatcher
	repo         Repository
	mail         mail.Port
	mailFrom     string
	pollInterval time.Duration
	window       time.Duration
	signer       *URLSigner // signs the unsubscribe link when configured
	serviceURL   string     // public notification service base URL
}

// NewDigestFlusher builds the flush worker.
func NewDigestFlusher(b DigestBatcher, repo Repository, m mail.Port, mailFrom string, pollInterval, window, staleness time.Duration, signer *URLSigner, serviceURL string) *DigestFlusher {
	if staleness > 0 {
		digestStalenessThreshold = staleness
	}
	return &DigestFlusher{batcher: b, repo: repo, mail: m, mailFrom: mailFrom, pollInterval: pollInterval, window: window, signer: signer, serviceURL: serviceURL}
}

// Run loops until ctx is cancelled. Never returns an error — per-iteration
// failures are logged and retried on the next tick.
func (f *DigestFlusher) Run(ctx context.Context) {
	if f == nil || f.batcher == nil || f.mail == nil {
		return
	}
	for {
		res, err := f.batcher.ClaimReady(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("digest claim failed", "err", err)
			if !sleep(ctx, f.pollInterval) {
				return
			}
			continue
		}
		switch res.Kind {
		case ClaimEmpty:
			if !sleep(ctx, f.pollInterval) {
				return
			}
		case ClaimWait:
			wait := res.Wait
			if wait > f.pollInterval {
				wait = f.pollInterval
			}
			if !sleep(ctx, wait) {
				return
			}
		case ClaimReady:
			if err := f.sendBatch(ctx, res.Batch); err != nil {
				slog.Error("digest send failed", "user", res.Batch.UserID, "err", err)
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// sendBatch ports egress::poll_email_digests: filter to still-eligible
// notifications, check type disable, then send one digest email.
func (f *DigestFlusher) sendBatch(ctx context.Context, batch *DigestBatch) error {
	ids := make([]uuid.UUID, 0, len(batch.Notifications))
	for _, n := range batch.Notifications {
		ids = append(ids, n.NotificationID)
	}
	eligible, err := f.repo.GetDigestEligibleNotificationIDs(ctx, batch.UserID, ids)
	if err != nil {
		return err
	}
	var kept []Row
	for _, n := range batch.Notifications {
		if eligible[n.NotificationID] {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 {
		slog.Info("skipping digest: all notifications read or deleted", "user", batch.UserID)
		return nil
	}

	to := emailPart(batch.UserID)
	if to == "" {
		return fmt.Errorf("digest recipient %q has no email part", batch.UserID)
	}
	// Subject matches Rust EmailDigestNotification ("You have {n} new
	// notifications on Macro"); the count is the full batch size.
	subject := fmt.Sprintf("You have %d new notifications on Macro", len(kept))
	unsub := f.digestUnsubscribeURL(batch.UserID)
	return f.mail.Send(ctx, mail.Message{
		To:       to,
		From:     f.mailFrom,
		Subject:  subject,
		TextBody: digestTextBody(kept, unsub),
		HTMLBody: digestHTMLBody(kept, unsub),
	})
}

// digestUnsubscribeURL builds the presigned preference-disable link that
// Rust's EmailDigestNotification embeds:
// {service_url}/user_notifications/preferences/email-digest-notification/disable?id={user}&sig={hmac}
// Empty when signing isn't configured.
func (f *DigestFlusher) digestUnsubscribeURL(userID string) string {
	if f.signer == nil || f.serviceURL == "" {
		return ""
	}
	u, err := url.Parse(f.serviceURL)
	if err != nil {
		return ""
	}
	u = appendURLPath(u, "/user_notifications/preferences/email-digest-notification/disable")
	q := u.Query()
	q.Set("id", userID)
	u.RawQuery = q.Encode()
	signed, err := f.signer.Sign(u)
	if err != nil {
		return ""
	}
	return signed.String()
}

func digestTextBody(rows []Row, unsubscribeURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You have %d new notifications on Macro:\n\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "- %s (%s %s)\n", r.NotificationEventType, r.EntityType, r.EntityID)
	}
	if unsubscribeURL != "" {
		fmt.Fprintf(&b, "\nUnsubscribe from digest emails: %s\n", unsubscribeURL)
	}
	return b.String()
}

func digestHTMLBody(rows []Row, unsubscribeURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>You have %d new notifications on Macro:</p><ul>", len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "<li>%s (%s %s)</li>",
			html.EscapeString(r.NotificationEventType),
			html.EscapeString(r.EntityType),
			html.EscapeString(r.EntityID))
	}
	b.WriteString("</ul>")
	if unsubscribeURL != "" {
		fmt.Fprintf(&b, `<p><a href="%s">Unsubscribe from digest emails</a></p>`,
			html.EscapeString(unsubscribeURL))
	}
	return b.String()
}
