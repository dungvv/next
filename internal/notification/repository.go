package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the notification persistence port. PGRepository implements it
// against the notification Postgres schema; a sqlc-generated implementation
// (pkg/store/notifdb) can be swapped in later.
type Repository interface {
	// ---- recipient filters ----
	GetMutedUsers(ctx context.Context, userIDs []string) (map[string]bool, error)
	GetUnsubscribedUsers(ctx context.Context, itemID string, userIDs []string) (map[string]bool, error)
	GetUsersWithTypeDisabled(ctx context.Context, eventType string, userIDs []string) (map[string]bool, error)

	// ---- notification rows ----
	// CreateNotification inserts the notification + per-user rows.
	// Returns the created rows, or nil when the notification already exists
	// (idempotent on uuid_to_write).
	CreateNotification(ctx context.Context, req SendRequest, serviceName string, apnsCollapseKey *string) ([]Row, error)
	UpdateSentStatus(ctx context.Context, notificationID uuid.UUID, userIDs []string) error
	MarkSeen(ctx context.Context, userID string, notificationIDs []uuid.UUID) ([]Row, error)
	MarkDone(ctx context.Context, userID string, notificationIDs []uuid.UUID, done bool) ([]Row, error)
	GetCollapseKeys(ctx context.Context, notificationIDs []uuid.UUID) (map[uuid.UUID]string, error)
	GetDigestEligibleNotificationIDs(ctx context.Context, userID string, notificationIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	ListUserNotifications(ctx context.Context, q ListQuery) ([]Row, error)
	GetUserNotificationByID(ctx context.Context, userID string, notificationID uuid.UUID) (*Row, error)
	DeleteUserNotification(ctx context.Context, userID string, notificationID uuid.UUID) error
	BulkDeleteUserNotifications(ctx context.Context, userID string, notificationIDs []uuid.UUID) error

	// ---- type preferences ----
	GetDisabledNotificationTypes(ctx context.Context, userID string) ([]DisabledNotificationType, error)
	DisableNotificationType(ctx context.Context, userID, eventType string) error
	EnableNotificationType(ctx context.Context, userID, eventType string) error

	// ---- device registry (owns what SNS platform endpoints hid) ----
	GetDeviceEndpoints(ctx context.Context, userIDs []string) (map[string][]DeviceEndpoint, error)
	GetDeviceEndpoint(ctx context.Context, token string, deviceType DeviceType) (string, error)
	// UpsertDevice mirrors notification_user_device_registration upsert on
	// device_endpoint conflict. The endpoint value must be produced by
	// endpointForToken so the UNIQUE(device_endpoint) constraint scopes
	// uniqueness to (device_type, token).
	UpsertDevice(ctx context.Context, userID, token, endpoint string, deviceType DeviceType) error
	DeleteUserDevicesByToken(ctx context.Context, userID, token string, deviceType DeviceType) ([]string, error)
	DeleteStaleDevicesByToken(ctx context.Context, token string, deviceType DeviceType, keepEndpoint string) ([]string, error)
	// DeleteDeviceByEndpoint deletes the registration whose stored endpoint
	// matches (Rust delete_device_by_endpoint). Callers must pass the
	// endpointForToken value — matching an endpoint can never delete a
	// sibling registration of a different device type.
	DeleteDeviceByEndpoint(ctx context.Context, endpoint string) error

	// ---- unsubscribe tables ----
	ListUnsubscribedItems(ctx context.Context, userID string) ([]UnsubscribeItem, error)
	AddItemUnsubscribe(ctx context.Context, userID, itemID, itemType string) error
	RemoveItemUnsubscribe(ctx context.Context, userID, itemID string) error
	MuteUser(ctx context.Context, userID string) error
	UnmuteUser(ctx context.Context, userID string) error
	// AddEmailUnsubscribe records an email in notification_email_unsubscribe
	// (INSERT ... ON CONFLICT DO NOTHING).
	AddEmailUnsubscribe(ctx context.Context, email string) error

	// ---- message receipts (provider message id → user_notification) ----
	RecordMessageReceipt(ctx context.Context, messageID, userID string, notificationID uuid.UUID) error
	MarkMessageFailed(ctx context.Context, messageID string) (userID string, notificationID uuid.UUID, err error)
	// DidAllMessagesFail reports whether every recorded message receipt for
	// the user+notification is marked failed (did_all_messages_fail_for_notification).
	DidAllMessagesFail(ctx context.Context, userID string, notificationID uuid.UUID) (bool, error)
}

// endpointForToken produces the value stored in the device_endpoint column
// for a (device type, token) pair. The column is UNIQUE and — unlike the
// Rust service where SNS endpoint ARNs were inherently per-platform — a bare
// token would collide across device types (e.g. an iOS app token and a VoIP
// token). Namespacing keeps registrations distinct so a VoIP upsert or
// cleanup never replaces or deletes the main-app registration.
func endpointForToken(token string, dType DeviceType) string {
	return string(dType) + ":" + token
}

// UnsubscribeItem is one row of GET /unsubscribe.
type UnsubscribeItem struct {
	ItemID   string `json:"item_id"`
	ItemType string `json:"item_type"`
}

// ListQuery carries pagination + filters for notification listing.
type ListQuery struct {
	UserID       string
	Limit        int
	CursorID     *uuid.UUID
	CursorTS     *time.Time
	EventItemIDs []string // optional filter on n.event_item_id
	States       []State  // empty = no state restriction (caller defaults to ActiveStates)
	Entities     []Entity // optional entity filter
	IncludeTypes []string // optional category filter (email, message, channel, ...)
}

// PGRepository is the pgx-backed Repository.
type PGRepository struct {
	db *pgxpool.Pool
}

// NewPGRepository builds a repository on the notification DB pool.
func NewPGRepository(db *pgxpool.Pool) *PGRepository {
	return &PGRepository{db: db}
}

// sqlb accumulates a query string with $n args.
type sqlb struct {
	strings.Builder
	args []any
}

func (b *sqlb) bind(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

func stringSet(rows pgx.Rows, err error) (map[string]bool, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

func (r *PGRepository) GetMutedUsers(ctx context.Context, userIDs []string) (map[string]bool, error) {
	rows, err := r.db.Query(ctx,
		`SELECT user_id FROM user_mute_notification WHERE user_id = ANY($1)`, userIDs)
	return stringSet(rows, err)
}

func (r *PGRepository) GetUnsubscribedUsers(ctx context.Context, itemID string, userIDs []string) (map[string]bool, error) {
	rows, err := r.db.Query(ctx,
		`SELECT user_id FROM user_notification_item_unsubscribe WHERE item_id = $1 AND user_id = ANY($2)`,
		itemID, userIDs)
	return stringSet(rows, err)
}

func (r *PGRepository) GetUsersWithTypeDisabled(ctx context.Context, eventType string, userIDs []string) (map[string]bool, error) {
	rows, err := r.db.Query(ctx,
		`SELECT user_id FROM user_notification_type_preference WHERE notification_event_type = $1 AND user_id = ANY($2)`,
		eventType, userIDs)
	return stringSet(rows, err)
}

// CreateNotification ports repository.rs create_notification: one tx that
// inserts the notification row (ON CONFLICT DO NOTHING) then the per-user
// rows, returning constructed Rows for downstream processing.
func (r *PGRepository) CreateNotification(ctx context.Context, req SendRequest, serviceName string, apnsCollapseKey *string) ([]Row, error) {
	var secondaryID, secondaryType *string
	if s := req.Req.SecondaryNotificationEntity; s != nil {
		secondaryID = &s.EntityID
		secondaryType = &s.EntityType
	}
	var senderID *string
	if s := req.Req.SenderID; s != nil {
		v := *s
		senderID = &v
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		INSERT INTO notification (
			id, notification_event_type, event_item_id, event_item_type,
			service_sender, metadata, sender_id, apns_collapse_key,
			secondary_event_item_id, secondary_event_item_type
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO NOTHING`,
		req.UUIDToWrite,
		req.Req.Notification.Tag,
		req.Req.NotificationEntity.EntityID,
		req.Req.NotificationEntity.EntityType,
		serviceName,
		req.Req.Notification.Content,
		senderID,
		apnsCollapseKey,
		secondaryID,
		secondaryType,
	)
	if err != nil {
		return nil, fmt.Errorf("insert notification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil // idempotent: already exists
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO user_notification (notification_id, user_id)
		SELECT $1, user_id FROM UNNEST($2::text[]) AS user_id
		ON CONFLICT (user_id, notification_id) DO NOTHING`,
		req.UUIDToWrite, req.Req.RecipientIDs,
	); err != nil {
		return nil, fmt.Errorf("insert user_notification: %w", err)
	}

	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		`SELECT created_at::timestamptz FROM notification WHERE id = $1`,
		req.UUIDToWrite).Scan(&createdAt); err != nil {
		return nil, fmt.Errorf("read notification created_at: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	rows := make([]Row, 0, len(req.Req.RecipientIDs))
	for _, recipient := range req.Req.RecipientIDs {
		rows = append(rows, Row{
			OwnerID:               recipient,
			NotificationID:        req.UUIDToWrite,
			NotificationEventType: req.Req.Notification.Tag,
			Entity:                req.Req.NotificationEntity,
			Sent:                  false,
			State:                 StateUnseen,
			CreatedAt:             createdAt,
			UpdatedAt:             createdAt,
			NotificationMetadata:  req.Req.Notification.Content,
			SenderID:              senderID,
		})
	}
	return rows, nil
}

func (r *PGRepository) UpdateSentStatus(ctx context.Context, notificationID uuid.UUID, userIDs []string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE user_notification SET sent = true WHERE notification_id = $1 AND user_id = ANY($2)`,
		notificationID, userIDs)
	return err
}

const updatedRowSelect = `
	SELECT
		updated.user_id AS owner_id,
		updated.notification_id,
		n.event_item_id,
		n.event_item_type,
		updated.sent,
		updated.state::text,
		updated.created_at::timestamptz,
		updated.seen_at::timestamptz AS viewed_at,
		NOW()::timestamptz AS updated_at,
		updated.deleted_at::timestamptz,
		n.metadata,
		n.notification_event_type,
		n.sender_id
	FROM updated
	JOIN notification n ON n.id = updated.notification_id
	ORDER BY array_position($2, updated.notification_id)`

func (r *PGRepository) MarkSeen(ctx context.Context, userID string, notificationIDs []uuid.UUID) ([]Row, error) {
	rows, err := r.db.Query(ctx, `
		WITH updated AS (
			UPDATE user_notification
			SET state = CASE WHEN state = 'unseen' THEN 'seen'::notification_state ELSE state END,
				seen_at = COALESCE(seen_at, NOW())
			WHERE user_id = $1 AND notification_id = ANY($2) AND deleted_at IS NULL
			RETURNING user_id, notification_id, sent, state, created_at, seen_at, deleted_at
		)`+updatedRowSelect,
		userID, notificationIDs)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

func (r *PGRepository) MarkDone(ctx context.Context, userID string, notificationIDs []uuid.UUID, done bool) ([]Row, error) {
	rows, err := r.db.Query(ctx, `
		WITH updated AS (
			UPDATE user_notification
			SET state = CASE
				WHEN $3 THEN 'done'::notification_state
				WHEN state = 'done' THEN 'seen'::notification_state
				ELSE state
			END
			WHERE user_id = $1 AND notification_id = ANY($2) AND deleted_at IS NULL
			RETURNING user_id, notification_id, sent, state, created_at, seen_at, deleted_at
		)`+updatedRowSelect,
		userID, notificationIDs, done)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

func scanRows(rows pgx.Rows) ([]Row, error) {
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var row Row
		var state string
		err := rows.Scan(
			&row.OwnerID,
			&row.NotificationID,
			&row.EntityID,
			&row.EntityType,
			&row.Sent,
			&state,
			&row.CreatedAt,
			&row.ViewedAt,
			&row.UpdatedAt,
			&row.DeletedAt,
			&row.NotificationMetadata,
			&row.NotificationEventType,
			&row.SenderID,
		)
		if err != nil {
			return nil, err
		}
		row.State = State(state)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *PGRepository) GetCollapseKeys(ctx context.Context, notificationIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, apns_collapse_key FROM notification WHERE id = ANY($1) AND apns_collapse_key IS NOT NULL`,
		notificationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		out[id] = key
	}
	return out, rows.Err()
}

func (r *PGRepository) GetDigestEligibleNotificationIDs(ctx context.Context, userID string, notificationIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	// Mirrors the Rust digest-eligibility check: notification still unseen,
	// not deleted, and the user has not disabled that event type.
	rows, err := r.db.Query(ctx, `
		SELECT un.notification_id
		FROM user_notification un
		JOIN notification n ON n.id = un.notification_id
		WHERE un.user_id = $1 AND un.notification_id = ANY($2)
		  AND un.deleted_at IS NULL AND un.state = 'unseen'
		  AND NOT EXISTS (
			SELECT 1 FROM user_notification_type_preference p
			WHERE p.user_id = un.user_id
			  AND p.notification_event_type = n.notification_event_type
		  )`,
		userID, notificationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// category SQL fragments, ported from push_include_types_filter.
const githubEventTypesSQL = `n.notification_event_type IN ('github_pr_status_changed', 'github_review_requested', 'github_pr_comment', 'github_pr_mention', 'github_pr_review', 'github_pr_check_run')`

var categoryClauses = map[string]string{
	"email":    `n.event_item_type = 'email_thread'`,
	"message":  `(n.notification_event_type IN ('channel_mention', 'channel_message_reply', 'channel_message_send') OR n.metadata ? 'messageId' OR n.metadata ? 'message_id')`,
	"channel":  `n.event_item_type = 'channel'`,
	"document": `n.event_item_type = 'document' AND COALESCE(n.metadata->>'subType', n.metadata->>'sub_type', '') <> 'task'`,
	"task":     `n.event_item_type = 'document' AND COALESCE(n.metadata->>'subType', n.metadata->>'sub_type', '') = 'task'`,
	"project":  `n.event_item_type = 'project'`,
	"chat":     `n.event_item_type = 'chat'`,
	"call":     `n.event_item_type = 'call'`,
	"github":   githubEventTypesSQL,
	"reminder": `n.event_item_type = 'reminder'`,
	"calendar": `n.event_item_type = 'calendar_event'`,
	"agent":    `n.event_item_type = 'agent_session'`,
}

// ListUserNotifications ports build_user_notifications_query.
func (r *PGRepository) ListUserNotifications(ctx context.Context, q ListQuery) ([]Row, error) {
	var b sqlb
	b.WriteString(`
		SELECT
			un.user_id AS owner_id,
			un.notification_id,
			n.event_item_id,
			n.event_item_type,
			un.sent,
			un.state::text,
			un.created_at::timestamptz,
			un.seen_at::timestamptz AS viewed_at,
			un.created_at::timestamptz AS updated_at,
			un.deleted_at::timestamptz,
			n.metadata,
			n.notification_event_type,
			n.sender_id
		FROM user_notification un
		JOIN notification n ON n.id = un.notification_id
		WHERE un.user_id = `)
	b.WriteString(b.bind(q.UserID))

	if len(q.EventItemIDs) > 0 {
		b.WriteString(" AND n.event_item_id = ANY(")
		b.WriteString(b.bind(q.EventItemIDs))
		b.WriteString(")")
	}

	b.WriteString(" AND un.deleted_at IS NULL")
	if len(q.States) > 0 {
		states := make([]string, len(q.States))
		for i, s := range q.States {
			states[i] = string(s)
		}
		b.WriteString(" AND un.state = ANY(")
		b.WriteString(b.bind(states))
		b.WriteString(")")
	}

	if len(q.IncludeTypes) > 0 {
		var clauses []string
		for _, t := range q.IncludeTypes {
			if c, ok := categoryClauses[t]; ok {
				clauses = append(clauses, "("+c+")")
			}
		}
		if len(clauses) > 0 {
			b.WriteString(" AND (")
			b.WriteString(strings.Join(clauses, " OR "))
			b.WriteString(")")
		}
	}

	if len(q.Entities) > 0 {
		b.WriteString(" AND (")
		for i, e := range q.Entities {
			if i > 0 {
				b.WriteString(" OR ")
			}
			b.WriteString("((n.event_item_type = ")
			b.WriteString(b.bind(e.EntityType))
			b.WriteString(" AND n.event_item_id = ")
			b.WriteString(b.bind(e.EntityID))
			b.WriteString(") OR (n.secondary_event_item_type = ")
			b.WriteString(b.bind(e.EntityType))
			b.WriteString(" AND n.secondary_event_item_id = ")
			b.WriteString(b.bind(e.EntityID))
			b.WriteString(")")
			if e.EntityType == "channel_message" {
				b.WriteString(" OR COALESCE(n.metadata->>'messageId', n.metadata->>'message_id', '') = ")
				b.WriteString(b.bind(e.EntityID))
			}
			b.WriteString(")")
		}
		b.WriteString(")")
	}

	if q.CursorTS != nil && q.CursorID != nil {
		b.WriteString(" AND (un.created_at, un.notification_id) < (")
		b.WriteString(b.bind(*q.CursorTS))
		b.WriteString(", ")
		b.WriteString(b.bind(*q.CursorID))
		b.WriteString(")")
	}

	b.WriteString(" ORDER BY un.created_at DESC, un.notification_id DESC LIMIT ")
	b.WriteString(b.bind(q.Limit))

	rows, err := r.db.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

func (r *PGRepository) GetUserNotificationByID(ctx context.Context, userID string, notificationID uuid.UUID) (*Row, error) {
	rows, err := r.db.Query(ctx, `
		SELECT
			un.user_id AS owner_id,
			un.notification_id,
			n.event_item_id,
			n.event_item_type,
			un.sent,
			un.state::text,
			un.created_at::timestamptz,
			un.seen_at::timestamptz AS viewed_at,
			un.created_at::timestamptz AS updated_at,
			un.deleted_at::timestamptz,
			n.metadata,
			n.notification_event_type,
			n.sender_id
		FROM user_notification un
		JOIN notification n ON n.id = un.notification_id
		WHERE un.user_id = $1 AND un.notification_id = $2 AND un.deleted_at IS NULL
		LIMIT 1`,
		userID, notificationID)
	if err != nil {
		return nil, err
	}
	out, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

func (r *PGRepository) DeleteUserNotification(ctx context.Context, userID string, notificationID uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE user_notification SET deleted_at = NOW() WHERE user_id = $1 AND notification_id = $2`,
		userID, notificationID)
	return err
}

func (r *PGRepository) BulkDeleteUserNotifications(ctx context.Context, userID string, notificationIDs []uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE user_notification SET deleted_at = NOW() WHERE user_id = $1 AND notification_id = ANY($2)`,
		userID, notificationIDs)
	return err
}

func (r *PGRepository) GetDisabledNotificationTypes(ctx context.Context, userID string) ([]DisabledNotificationType, error) {
	rows, err := r.db.Query(ctx,
		`SELECT user_id, notification_event_type FROM user_notification_type_preference WHERE user_id = $1`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DisabledNotificationType
	for rows.Next() {
		var d DisabledNotificationType
		if err := rows.Scan(&d.UserID, &d.NotificationEventType); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *PGRepository) DisableNotificationType(ctx context.Context, userID, eventType string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO user_notification_type_preference (user_id, notification_event_type)
		VALUES ($1, $2) ON CONFLICT (user_id, notification_event_type) DO NOTHING`,
		userID, eventType)
	return err
}

func (r *PGRepository) EnableNotificationType(ctx context.Context, userID, eventType string) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM user_notification_type_preference WHERE user_id = $1 AND notification_event_type = $2`,
		userID, eventType)
	return err
}

func (r *PGRepository) GetDeviceEndpoints(ctx context.Context, userIDs []string) (map[string][]DeviceEndpoint, error) {
	rows, err := r.db.Query(ctx,
		`SELECT user_id, device_token, device_type::text FROM notification_user_device_registration WHERE user_id = ANY($1)`,
		userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]DeviceEndpoint{}
	for rows.Next() {
		var userID, token, devType string
		if err := rows.Scan(&userID, &token, &devType); err != nil {
			return nil, err
		}
		out[userID] = append(out[userID], DeviceEndpoint{Type: DeviceType(devType), Token: token})
	}
	return out, rows.Err()
}

func (r *PGRepository) GetDeviceEndpoint(ctx context.Context, token string, deviceType DeviceType) (string, error) {
	var endpoint string
	err := r.db.QueryRow(ctx,
		`SELECT device_endpoint FROM notification_user_device_registration WHERE device_token = $1 AND device_type = $2 LIMIT 1`,
		token, string(deviceType)).Scan(&endpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return endpoint, err
}

func (r *PGRepository) UpsertDevice(ctx context.Context, userID, token, endpoint string, deviceType DeviceType) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO notification_user_device_registration (id, user_id, device_token, device_endpoint, device_type)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (device_endpoint) DO UPDATE SET user_id = $2, device_token = $3, device_type = $5, updated_at = NOW()`,
		uuid.New(), userID, token, endpoint, string(deviceType))
	return err
}

func (r *PGRepository) DeleteUserDevicesByToken(ctx context.Context, userID, token string, deviceType DeviceType) ([]string, error) {
	rows, err := r.db.Query(ctx,
		`DELETE FROM notification_user_device_registration WHERE user_id = $1 AND device_token = $2 AND device_type = $3 RETURNING device_endpoint`,
		userID, token, string(deviceType))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *PGRepository) DeleteStaleDevicesByToken(ctx context.Context, token string, deviceType DeviceType, keepEndpoint string) ([]string, error) {
	rows, err := r.db.Query(ctx,
		`DELETE FROM notification_user_device_registration WHERE device_token = $1 AND device_type = $2 AND device_endpoint != $3 RETURNING device_endpoint`,
		token, string(deviceType), keepEndpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *PGRepository) DeleteDeviceByEndpoint(ctx context.Context, endpoint string) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM notification_user_device_registration WHERE device_endpoint = $1`,
		endpoint)
	return err
}

func (r *PGRepository) ListUnsubscribedItems(ctx context.Context, userID string) ([]UnsubscribeItem, error) {
	rows, err := r.db.Query(ctx,
		`SELECT item_id, item_type FROM user_notification_item_unsubscribe WHERE user_id = $1`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnsubscribeItem{}
	for rows.Next() {
		var u UnsubscribeItem
		if err := rows.Scan(&u.ItemID, &u.ItemType); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *PGRepository) AddItemUnsubscribe(ctx context.Context, userID, itemID, itemType string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO user_notification_item_unsubscribe (user_id, item_id, item_type)
		VALUES ($1, $2, $3) ON CONFLICT (user_id, item_id) DO NOTHING`,
		userID, itemID, itemType)
	return err
}

func (r *PGRepository) RemoveItemUnsubscribe(ctx context.Context, userID, itemID string) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM user_notification_item_unsubscribe WHERE user_id = $1 AND item_id = $2`,
		userID, itemID)
	return err
}

func (r *PGRepository) MuteUser(ctx context.Context, userID string) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO user_mute_notification (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`,
		userID)
	return err
}

func (r *PGRepository) UnmuteUser(ctx context.Context, userID string) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM user_mute_notification WHERE user_id = $1`, userID)
	return err
}

func (r *PGRepository) RecordMessageReceipt(ctx context.Context, messageID, userID string, notificationID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO notification_message_receipt (message_id, user_id, notification_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (message_id) DO NOTHING`,
		messageID, userID, notificationID)
	return err
}

func (r *PGRepository) MarkMessageFailed(ctx context.Context, messageID string) (string, uuid.UUID, error) {
	var userID string
	var notifID uuid.UUID
	err := r.db.QueryRow(ctx, `
		UPDATE notification_message_receipt
		SET failed = TRUE, failed_at = NOW()
		WHERE message_id = $1
		RETURNING user_id, notification_id`,
		messageID).Scan(&userID, &notifID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, fmt.Errorf("no receipt for message_id %s", messageID)
	}
	return userID, notifID, err
}

func (r *PGRepository) DidAllMessagesFail(ctx context.Context, userID string, notificationID uuid.UUID) (bool, error) {
	var remaining int
	err := r.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM notification_message_receipt
		WHERE user_id = $1 AND notification_id = $2 AND failed = FALSE`,
		userID, notificationID).Scan(&remaining)
	if err != nil {
		return false, err
	}
	return remaining == 0, nil
}

func (r *PGRepository) AddEmailUnsubscribe(ctx context.Context, email string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO notification_email_unsubscribe (email)
		VALUES ($1) ON CONFLICT (email) DO NOTHING`,
		email)
	return err
}

// interface conformance
var _ Repository = (*PGRepository)(nil)
