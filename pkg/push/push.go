// Package push provides the PushPort interface and direct-provider adapters
// for FCM (HTTP v1) and APNs (token auth via apns2). It replaces the SNS
// platform-application layer: callers register raw device tokens and send
// straight to the provider.
package push

import (
	"context"
	"errors"
	"fmt"
)

// Platform identifies a device push platform.
type Platform string

const (
	// PlatformIOS is Apple Push Notification service.
	PlatformIOS Platform = "ios"
	// PlatformAndroid is Firebase Cloud Messaging.
	PlatformAndroid Platform = "android"
	// PlatformIOSVoIP is APNs VoIP (PushKit / CallKit).
	PlatformIOSVoIP Platform = "iosvoip"
)

// ParsePlatform parses a platform string (as stored in the
// notification_device_type_option Postgres enum).
func ParsePlatform(s string) (Platform, error) {
	switch Platform(s) {
	case PlatformIOS, PlatformAndroid, PlatformIOSVoIP:
		return Platform(s), nil
	default:
		return "", fmt.Errorf("push: unknown platform %q", s)
	}
}

// PushType controls the delivery semantics of a push.
type PushType string

const (
	// PushTypeAlert is a visible alert notification.
	PushTypeAlert PushType = "alert"
	// PushTypeBackground is a silent content-available push.
	PushTypeBackground PushType = "background"
	// PushTypeVoIP is a PushKit/CallKit push (APNs only).
	PushTypeVoIP PushType = "voip"
)

// Message is a provider-agnostic push notification.
type Message struct {
	// Token is the raw provider device token (APNs device token or FCM
	// registration token) — this is what SNS hid behind endpoint ARNs.
	Token string
	// PushType selects alert vs. background/voip delivery.
	PushType PushType
	// Title is the alert title (APNs aps.alert.title / FCM notification.title).
	Title string
	// Body is the alert body.
	Body string
	// CollapseKey coalesces notifications on the device (apns-collapse-id /
	// FCM collapse_key).
	CollapseKey string
	// APS is the verbatim `aps` dictionary supplied by the producer (APNs
	// only — the Rust service serialized APNSPushNotification as-is). When
	// nil, the APNs adapter builds aps from PushType/Title/Body.
	APS map[string]any
	// Data is custom payload merged into the provider message (flattened into
	// the APNs root object; sent as FCM `data` string map). Must not contain
	// the "aps" key.
	Data map[string]any
	// MutableContent sets aps.mutable-content=1 (APNs only).
	MutableContent bool
	// Topic overrides the default APNs topic (bundle id); empty = default.
	Topic string
}

// ErrTokenUnregistered is returned (wrapped) when the provider reports the
// device token is permanently invalid: FCM `UNREGISTERED`/NotRegistered or
// APNs 410/`Unregistered`/`BadDeviceToken`. Callers should delete the token
// from the device registry.
var ErrTokenUnregistered = errors.New("push: device token is unregistered")

// Port is the outbound port for a single push provider.
// It returns the provider-assigned message ID on success.
type Port interface {
	Send(ctx context.Context, msg Message) (string, error)
}

// Sender routes a Message to the correct provider by platform.
// Providers may be nil (unconfigured); sending to a nil provider is an error.
type Sender struct {
	FCM  Port
	APNs Port
	// VoIP sends PushTypeVoIP messages; defaults to APNs when nil.
	VoIP Port
}

// Send dispatches msg to the provider for platform.
func (s *Sender) Send(ctx context.Context, platform Platform, msg Message) (string, error) {
	var p Port
	switch platform {
	case PlatformAndroid:
		p = s.FCM
	case PlatformIOS:
		p = s.APNs
	case PlatformIOSVoIP:
		if s.VoIP != nil {
			p = s.VoIP
		} else {
			p = s.APNs
		}
	default:
		return "", fmt.Errorf("push: no provider for platform %q", platform)
	}
	if p == nil {
		return "", fmt.Errorf("push: provider for platform %q is not configured", platform)
	}
	return p.Send(ctx, msg)
}
