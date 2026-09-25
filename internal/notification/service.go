package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/push"
)

// Service implements the ingress + reader side of the notification domain:
// filter → persist → publish per-user delivery messages, plus the read/update
// surface used by the HTTP handlers.
type Service struct {
	repo        Repository
	js          jetstream.JetStream
	nc          *nats.Conn
	status      *StatusPublisher
	gateway     *GatewayClient
	pushes      *push.Sender
	digests     DigestBatcher
	serviceName string
	// digestGate runs the Rust StateMachineDriverA decision per recipient.
	gate digestGatePorts
}

// SendNotification ports NotificationIngressService::send_notification_impl:
// filter recipients → idempotent insert → publish per-user delivery messages.
// Returns the notification id and the recipients that were actually notified;
// (id, nil) nil-rows means the notification already existed (idempotent).
func (s *Service) SendNotification(ctx context.Context, req SendRequest) (*uuid.UUID, []string, error) {
	if req.UUIDToWrite == uuid.Nil {
		req.UUIDToWrite = uuid.New()
	}
	// Default realtime send when no channel builders are set (Rust callers
	// set send_conn_gateway explicitly; tolerate producers that omit it).
	if !req.SendConnGateway && req.BuildApns == nil && req.BuildEmail == nil {
		req.SendConnGateway = true
	}

	filtered, err := s.filterRecipients(ctx, req.Req)
	if err != nil {
		return nil, nil, err
	}
	if len(filtered) == 0 {
		return nil, nil, nil
	}
	req.Req.RecipientIDs = filtered

	var collapseKey *string
	if req.BuildApns != nil && req.BuildApns.Attr.CollapseKey != "" {
		k := req.BuildApns.Attr.CollapseKey
		collapseKey = &k
	}

	rows, err := s.repo.CreateNotification(ctx, req, s.serviceName, collapseKey)
	if err != nil {
		return nil, nil, err
	}
	if rows == nil {
		// Idempotent redelivery: notification already existed.
		return &req.UUIDToWrite, nil, nil
	}

	// Digest gate (Rust StateMachineDriverA::ingest) — runs for every
	// recipient regardless of requested channels, and may queue a digest
	// batch immediately (push-disabled + offline beyond threshold).
	decisions, err := s.gate.ingest(ctx, rows)
	if err != nil {
		return nil, nil, fmt.Errorf("digest gate: %w", err)
	}

	// Per-user delivery messages on notifications.delivery.
	for _, row := range rows {
		msg := DeliveryMessage{
			NotificationID: req.UUIDToWrite,
			UserID:         row.OwnerID,
			Row:            row,
			Realtime:       req.SendConnGateway,
			// digest_deferred only rides on push-carrying messages (Rust
			// attaches digest_state inside the APNS queue message).
			DigestDeferred: decisions[row.OwnerID] == digestDeferred && req.BuildApns != nil,
		}
		if req.BuildApns != nil {
			title, body := req.BuildApns.Notif.AlertText()
			msg.Push = &PushSpec{
				PushType:    req.BuildApns.Attr.NormalizedPushType(),
				CollapseKey: req.BuildApns.Attr.CollapseKey,
				APS:         req.BuildApns.Notif.Aps,
				Data:        req.BuildApns.Notif.Data,
				Title:       title,
				Body:        body,
			}
		}
		if req.BuildEmail != nil {
			msg.Email = &req.BuildEmail.Content
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal delivery message: %w", err)
		}
		if _, err := s.js.Publish(ctx, "notifications.delivery", raw); err != nil {
			return nil, nil, fmt.Errorf("publish delivery: %w", err)
		}
	}

	return &req.UUIDToWrite, filtered, nil
}

// filterRecipients removes the sender, muted users, item-unsubscribed users,
// and users who disabled this notification type — the same exclusions as
// Rust SendNotificationRequest::update_recipients.
func (s *Service) filterRecipients(ctx context.Context, req Request) ([]string, error) {
	ids := dedupe(req.RecipientIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	muted, err := s.repo.GetMutedUsers(ctx, ids)
	if err != nil {
		return nil, err
	}
	unsubscribed, err := s.repo.GetUnsubscribedUsers(ctx, req.NotificationEntity.EntityID, ids)
	if err != nil {
		return nil, err
	}
	disabled, err := s.repo.GetUsersWithTypeDisabled(ctx, req.Notification.Tag, ids)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		switch {
		case req.SenderID != nil && *req.SenderID == id:
		case muted[id]:
		case unsubscribed[id]:
		case disabled[id]:
		default:
			out = append(out, id)
		}
	}
	return out, nil
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// ---- Reader surface ----

// UpdateNotifications applies seen/done/undone and mirrors the Rust flow:
// publish realtime status updates, then clear delivered pushes when the
// action requires it.
func (s *Service) UpdateNotifications(ctx context.Context, userID string, ids []uuid.UUID, status NotificationStatus) ([]Row, error) {
	var changed []Row
	var err error
	switch status.Kind {
	case StatusSeen:
		changed, err = s.repo.MarkSeen(ctx, userID, ids)
	case StatusDone:
		changed, err = s.repo.MarkDone(ctx, userID, ids, true)
	case StatusUndone:
		changed, err = s.repo.MarkDone(ctx, userID, ids, false)
	default:
		return nil, fmt.Errorf("unknown notification status")
	}
	if err != nil {
		return nil, err
	}

	if len(changed) > 0 {
		if err := s.status.PublishPatches(userID, changed); err != nil {
			slog.Warn("publish status update failed", "err", err)
		}
	}

	if status.ShouldClearPush() {
		if err := s.clearPushes(ctx, userID, ids); err != nil {
			slog.Warn("clear push notifications failed", "err", err)
		}
	}
	return changed, nil
}

// NotificationStatus is the requested status change.
type NotificationStatus struct {
	Kind StatusKind
}

type StatusKind int

const (
	StatusSeen StatusKind = iota
	StatusDone
	StatusUndone
)

// ShouldClearPush mirrors NotificationAction::should_clear_push_notifications:
// seen + done clear; reopen (undone) does not.
func (s NotificationStatus) ShouldClearPush() bool {
	return s.Kind == StatusSeen || s.Kind == StatusDone
}

// clearPushes sends silent background pushes carrying the notification's
// collapse key so devices dismiss the already-delivered notification.
func (s *Service) clearPushes(ctx context.Context, userID string, ids []uuid.UUID) error {
	if s.pushes == nil || s.pushes.APNs == nil {
		return nil
	}
	keys, err := s.repo.GetCollapseKeys(ctx, ids)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	devices, err := s.repo.GetDeviceEndpoints(ctx, []string{userID})
	if err != nil {
		return err
	}
	var iosTokens []string
	for _, ep := range devices[userID] {
		if ep.Type == DeviceIOS {
			iosTokens = append(iosTokens, ep.Token)
		}
	}
	if len(iosTokens) == 0 {
		return nil
	}
	for _, key := range keys {
		for _, token := range iosTokens {
			_, err := s.pushes.Send(ctx, push.PlatformIOS, push.Message{
				Token:       token,
				PushType:    push.PushTypeBackground,
				CollapseKey: key,
				Data:        map[string]any{"identifier": key},
			})
			if err != nil {
				if push.IsUnregistered(err) {
					_ = s.repo.DeleteDeviceByEndpoint(ctx, endpointForToken(token, DeviceIOS))
				}
				slog.Warn("clear push failed", "token", token, "err", err)
			}
		}
	}
	return nil
}

// ListNotifications returns a page of rows plus the next cursor.
func (s *Service) ListNotifications(ctx context.Context, q ListQuery) ([]Row, string, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	q.Limit = limit
	rows, err := s.repo.ListUserNotifications(ctx, q)
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(rows) == limit {
		last := rows[len(rows)-1]
		next, err = encodeCursor(last.NotificationID, limit, last.CreatedAt)
		if err != nil {
			return nil, "", err
		}
	}
	return rows, next, nil
}

// RegisterDevice stores a raw provider device token. The endpoint column
// carries a type-namespaced token (endpointForToken), so the
// UNIQUE(device_endpoint) constraint enforces per-(type, token) identity —
// the same token may be registered once per device type without clobbering
// a sibling registration.
func (s *Service) RegisterDevice(ctx context.Context, userID, token string, deviceType DeviceType) error {
	if token == "" {
		return errors.New("device token is required")
	}
	endpoint := endpointForToken(token, deviceType)
	if err := s.repo.UpsertDevice(ctx, userID, token, endpoint, deviceType); err != nil {
		return err
	}
	// A (token, type) pair maps to a single registration: drop rows (e.g.
	// another user's) that share it but point at a different endpoint value.
	_, err := s.repo.DeleteStaleDevicesByToken(ctx, token, deviceType, endpoint)
	return err
}

// UnregisterDevice deletes the user's registration for a token+type.
// Succeeds when nothing matched (idempotent for logout flows).
func (s *Service) UnregisterDevice(ctx context.Context, userID, token string, deviceType DeviceType) error {
	_, err := s.repo.DeleteUserDevicesByToken(ctx, userID, token, deviceType)
	return err
}

// blockableNotificationTypes mirrors BLOCKABLE_NOTIFICATIONS in
// services/notification_service (the user-disableable event types).
var blockableNotificationTypes = map[string]bool{
	"email-digest-notification": true,
	"new_email":                 true, "ai_response": true,
	"channel_message_send": true, "channel_mention": true,
	"channel_message_reply": true, "document_mention": true,
	"github_pr_status_changed": true, "github_review_requested": true,
	"github_pr_comment": true, "github_pr_mention": true, "github_pr_review": true,
	"task_assigned": true, "mentioned_in_document_comment": true,
	"replied_to_document_comment_thread": true, "commented_on_document": true,
	"calendar_event_reminder": true,
	"agent_session_settled":   true, "agent_session_waiting_for_input": true,
	"agent_session_mentioned": true,
}

// IsBlockable reports whether a notification event type can be user-disabled.
func IsBlockable(eventType string) bool {
	return blockableNotificationTypes[eventType]
}
