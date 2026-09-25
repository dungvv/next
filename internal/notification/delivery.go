package notification

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/macro-inc/macro/pkg/mail"
	"github.com/macro-inc/macro/pkg/push"
)

// Delivery orchestrates per-user notification delivery:
// realtime (gateway) → explicit email → mobile push → digest email fallback.
// It is the egress side of the Rust notification service.
type Delivery struct {
	repo     Repository
	nc       *nats.Conn
	gateway  *GatewayClient
	pushes   *push.Sender
	digests  DigestBatcher
	mail     mail.Port
	mailFrom string
	window   time.Duration // digest batch window (Rust DriverB/C digest_window = 30m)
}

// deliveryStatus is the per-user outcome published on
// notifications.status.<user_id> after each delivery attempt.
type deliveryStatus struct {
	Type           string `json:"type"` // "notification_delivery_status"
	NotificationID string `json:"notification_id"`
	UserID         string `json:"user_id"`
	Channel        string `json:"channel"` // realtime | push | email | digest
	Status         string `json:"status"`  // delivered | queued_digest | failed
}

func (d *Delivery) publishStatus(msg DeliveryMessage, channel, status string) {
	if d.nc == nil {
		return
	}
	payload, err := json.Marshal(deliveryStatus{
		Type:           "notification_delivery_status",
		NotificationID: msg.NotificationID.String(),
		UserID:         msg.UserID,
		Channel:        channel,
		Status:         status,
	})
	if err != nil {
		return
	}
	if err := d.nc.Publish("notifications.status."+msg.UserID, payload); err != nil {
		slog.Warn("publish delivery status failed", "err", err)
	}
}

// Deliver processes one per-user delivery message.
//
// Rust delivers each channel on its own SQS message (connection gateway,
// iOS push, email) so a failure in one channel retries independently.
// The Go port collapses them into a single delivery message; transient
// channel errors are collected and returned for JetStream redelivery, while
// push failures are terminal per attempt (Rust deletes the iOS message when
// all its sends fail — the digest batch is the compensating action).
func (d *Delivery) Deliver(ctx context.Context, msg DeliveryMessage) error {
	var transient []error
	online := false

	// 1. Realtime — the gateway reports whether the user is online. The
	// payload is the Rust RealtimeNotif shape (notification_id, no owner_id).
	if msg.Realtime && d.gateway != nil {
		onlineSet, err := d.gateway.Send(ctx, []string{msg.UserID}, "notification", msg.Row.realtimeWire())
		if err != nil {
			slog.Warn("gateway send failed", "user", msg.UserID, "err", err)
			transient = append(transient, err)
		} else {
			online = onlineSet[msg.UserID]
		}
	}

	// 2. Explicit email channel (build_email). In Rust this is a separate
	// queue message — it is delivered regardless of whether the user is
	// online, so it must run before the online early-return below.
	if msg.Email != nil && d.mail != nil {
		if err := d.mail.Send(ctx, mail.Message{
			To:       emailPart(msg.UserID),
			From:     d.mailFrom,
			Subject:  msg.Email.Subject,
			TextBody: msg.Email.Body,
		}); err != nil {
			slog.Warn("notification email failed", "user", msg.UserID, "err", err)
			transient = append(transient, err)
		} else {
			d.publishStatus(msg, "email", "delivered")
		}
	}

	// 3. Online users got the realtime frame; push only runs for offline
	// users. Explicit email above is unaffected.
	if online {
		d.publishStatus(msg, "realtime", "delivered")
		if err := d.repo.UpdateSentStatus(ctx, msg.NotificationID, []string{msg.UserID}); err != nil {
			return err
		}
		return errors.Join(transient...)
	}

	// 4. Push to offline users. Each endpoint is tried once; per the Rust
	// state machine (StateMachineDriverB) a digest is queued only when the
	// ingress gate deferred the decision (DigestDeferred) AND every push
	// attempt failed.
	sent := 0
	if msg.Push != nil && d.pushes != nil {
		devices, err := d.repo.GetDeviceEndpoints(ctx, []string{msg.UserID})
		if err != nil {
			return err
		}
		aps, data := msg.Push.resolve()
		for _, ep := range devices[msg.UserID] {
			platform, pushType, ok := endpointTarget(ep, msg.Push.PushType)
			if !ok {
				continue
			}
			messageID, err := d.pushes.Send(ctx, platform, push.Message{
				Token:       ep.Token,
				PushType:    pushType,
				CollapseKey: msg.Push.CollapseKey,
				Title:       msg.Push.Title,
				Body:        msg.Push.Body,
				APS:         aps,
				Data:        data,
			})
			if err != nil {
				slog.Warn("push send failed", "user", msg.UserID, "platform", platform, "err", err)
				if push.IsUnregistered(err) {
					// Delete only this type's registration — the endpoint
					// value is type-namespaced (endpointForToken), so a
					// failing registration can never delete a sibling
					// registration of a different device type that shares
					// the raw token.
					if derr := d.repo.DeleteDeviceByEndpoint(ctx, endpointForToken(ep.Token, ep.Type)); derr != nil {
						slog.Error("delete dead device failed", "err", derr)
					}
				}
				continue
			}
			sent++
			if messageID != "" {
				if err := d.repo.RecordMessageReceipt(ctx, messageID, msg.UserID, msg.NotificationID); err != nil {
					slog.Warn("record message receipt failed", "err", err)
				}
			}
		}
	}

	if sent > 0 {
		d.publishStatus(msg, "push", "delivered")
		if err := d.repo.UpdateSentStatus(ctx, msg.NotificationID, []string{msg.UserID}); err != nil {
			return err
		}
		return errors.Join(transient...)
	}

	// 5. Every push endpoint failed → digest email fallback, but only when
	// the ingress gate marked this user Indeterminate (push enabled, account
	// exists, type not block-listed, ≥1 iOS endpoint at ingress).
	if msg.DigestDeferred {
		d.queueDigest(ctx, msg)
	}
	return errors.Join(transient...)
}

// endpointTarget selects the provider + push type for a device endpoint.
// VoIP endpoints (PushKit) receive only voip pushes; alert/background pushes
// go to regular ios/android endpoints only — mirroring the Rust split where
// normal pushes are filtered to DeviceEndpoint::Ios and VoIP pushes go
// exclusively to DeviceEndpoint::IosVoip via a dedicated service.
func endpointTarget(ep DeviceEndpoint, pushType string) (push.Platform, push.PushType, bool) {
	switch ep.Type {
	case DeviceIOS:
		if pushType == "voip" {
			return "", "", false
		}
		return push.PlatformIOS, push.PushType(pushType), true
	case DeviceAndroid:
		if pushType == "voip" {
			return "", "", false
		}
		return push.PlatformAndroid, push.PushType(pushType), true
	case DeviceIOSVoIP:
		if pushType != "voip" {
			return "", "", false
		}
		return push.PlatformIOSVoIP, push.PushTypeVoIP, true
	}
	return "", "", false
}

// queueDigest batches the notification for a digest email unless the type is
// block-listed (new_email, invites) or no batcher is configured.
func (d *Delivery) queueDigest(ctx context.Context, msg DeliveryMessage) {
	if d.digests == nil || digestEmailBlockList[msg.Row.NotificationEventType] {
		return
	}
	if err := d.digests.Add(ctx, msg.Row, d.window); err != nil {
		slog.Error("digest add failed", "user", msg.UserID, "err", err)
		return
	}
	d.publishStatus(msg, "digest", "queued_digest")
}

// PushEventHandler processes provider feedback (push_events subject or the
// HTTP webhook): failures mark receipts and dead tokens are removed.
type PushEventHandler struct {
	repo    Repository
	digests DigestBatcher
	window  time.Duration // Rust DriverC digest_window = 30m
}

// NewPushEventHandler builds the handler.
func NewPushEventHandler(repo Repository, digests DigestBatcher, window time.Duration) *PushEventHandler {
	return &PushEventHandler{repo: repo, digests: digests, window: window}
}

// Handle processes one PushEvent, porting
// PushNotificationEventService::handle_event: the device registration is
// deleted for both DeliveryFailure and EndpointDeleted events, and a
// delivery failure is recorded in the digest failure state machine
// (StateMachineDriverC) which batches the notification once every recorded
// receipt for the user+notification has failed.
func (h *PushEventHandler) Handle(ctx context.Context, ev PushEvent) error {
	token := ev.Endpoint
	if token == "" {
		token = ev.Token
	}

	switch ev.EventType {
	case PushEventDeliveryFailure, PushEventEndpointDeleted:
		if token != "" {
			// Scope the delete to the device type when the publisher
			// supplied one (a VoIP endpoint must never delete the main-app
			// registration); unknown type falls back to a bare-token match.
			endpoint := token
			if ev.DeviceType != "" {
				endpoint = endpointForToken(token, DeviceType(ev.DeviceType))
			}
			if err := h.repo.DeleteDeviceByEndpoint(ctx, endpoint); err != nil {
				return err
			}
		}
	}

	if ev.EventType != PushEventDeliveryFailure || ev.MessageID == "" {
		return nil
	}

	// StateMachineDriverC::mark_message_as_failed — errors are logged and
	// swallowed by the Rust caller (inspect_err().ok()), so mirror that.
	userID, notificationID, err := h.repo.MarkMessageFailed(ctx, ev.MessageID)
	if err != nil {
		slog.Warn("mark message failed", "message_id", ev.MessageID, "err", err)
		return nil
	}
	if userID == "" {
		return nil
	}
	allFailed, err := h.repo.DidAllMessagesFail(ctx, userID, notificationID)
	if err != nil {
		slog.Warn("did_all_messages_fail check failed", "err", err)
		return nil
	}
	if !allFailed {
		return nil
	}
	row, err := h.repo.GetUserNotificationByID(ctx, userID, notificationID)
	if err != nil || row == nil {
		if err != nil {
			slog.Warn("get notification for digest failed", "err", err)
		}
		return nil
	}
	// DriverC has no block-list check — a queued digest is gated solely by
	// the all-receipts-failed condition; parity with Rust.
	if h.digests != nil {
		if err := h.digests.Add(ctx, *row, h.window); err != nil {
			slog.Error("digest add after push failure", "err", err)
		}
	}
	return nil
}
