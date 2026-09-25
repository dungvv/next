package push

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/token"
)

// APNsConfig configures the APNs token-auth adapter.
type APNsConfig struct {
	// KeyFile is the path to the .p8 APNs auth key.
	KeyFile string
	// KeyID is the 10-char key identifier for the .p8 file.
	KeyID string
	// TeamID is the Apple developer team ID.
	TeamID string
	// BundleID is the app bundle ID used as the APNs topic.
	BundleID string
	// VoIPBundleID is the topic for PushKit pushes (typically BundleID+".voip").
	// Defaults to BundleID+".voip" when empty.
	VoIPBundleID string
	// Sandbox uses the APNs development environment.
	Sandbox bool
}

// APNs sends pushes directly to Apple Push Notification service.
type APNs struct {
	client       *apns2.Client
	bundleID     string
	voipBundleID string
}

// NewAPNs builds an APNs adapter from a .p8 token key.
func NewAPNs(cfg APNsConfig) (*APNs, error) {
	if cfg.KeyFile == "" || cfg.KeyID == "" || cfg.TeamID == "" || cfg.BundleID == "" {
		return nil, fmt.Errorf("push: APNs requires key file, key id, team id, and bundle id")
	}
	key, err := token.AuthKeyFromFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("push: load APNs key: %w", err)
	}
	client := apns2.NewTokenClient(&token.Token{
		AuthKey: key,
		KeyID:   cfg.KeyID,
		TeamID:  cfg.TeamID,
	})
	if cfg.Sandbox {
		client = client.Development()
	} else {
		client = client.Production()
	}
	voip := cfg.VoIPBundleID
	if voip == "" {
		voip = cfg.BundleID + ".voip"
	}
	return &APNs{client: client, bundleID: cfg.BundleID, voipBundleID: voip}, nil
}

// Send implements Port. It returns the apns-id on success.
func (a *APNs) Send(ctx context.Context, msg Message) (string, error) {
	// The Rust service serialized the producer's APNSPushNotification
	// verbatim (its `aps` dict plus flattened custom data) as the APNS
	// platform message — honor a verbatim aps when supplied and only
	// synthesize one from Title/Body/PushType otherwise.
	aps := msg.APS
	if aps == nil {
		aps = map[string]any{}
		switch msg.PushType {
		case PushTypeBackground:
			aps["content-available"] = 1
		case PushTypeVoIP:
			// SNS required an empty aps object for APNS_VOIP payloads; keep
			// the same shape for direct APNs delivery.
		default:
			alert := map[string]any{}
			if msg.Title != "" {
				alert["title"] = msg.Title
			}
			if msg.Body != "" {
				alert["body"] = msg.Body
			}
			aps["alert"] = alert
			aps["sound"] = "default"
			if msg.MutableContent {
				aps["mutable-content"] = 1
			}
		}
	}

	payload := map[string]any{"aps": aps}
	for k, v := range msg.Data {
		if k == "aps" {
			continue
		}
		payload[k] = v
	}

	n := &apns2.Notification{
		DeviceToken: msg.Token,
		Payload:     payload,
		CollapseID:  msg.CollapseKey,
	}
	switch msg.PushType {
	case PushTypeBackground:
		n.PushType = apns2.PushTypeBackground
		n.Priority = apns2.PriorityLow
		n.Topic = firstNonEmpty(msg.Topic, a.bundleID)
	case PushTypeVoIP:
		n.PushType = apns2.PushTypeVOIP
		n.Priority = apns2.PriorityHigh
		n.Topic = firstNonEmpty(msg.Topic, a.voipBundleID)
		// TTL 0 — "deliver now or drop"; APNs must not replay a dead call
		// ring when the device reconnects (Rust APNS_VOIP.TTL=0).
		n.Expiration = time.Unix(0, 0)
	default:
		n.PushType = apns2.PushTypeAlert
		n.Priority = apns2.PriorityHigh
		n.Topic = firstNonEmpty(msg.Topic, a.bundleID)
	}

	res, err := a.client.PushWithContext(ctx, n)
	if err != nil {
		return "", fmt.Errorf("push: APNs request: %w", err)
	}
	if res.Sent() {
		return res.ApnsID, nil
	}
	if res.StatusCode == 410 ||
		res.Reason == apns2.ReasonUnregistered ||
		res.Reason == apns2.ReasonBadDeviceToken ||
		res.Reason == apns2.ReasonDeviceTokenNotForTopic {
		return "", fmt.Errorf("%w: %s", ErrTokenUnregistered, res.Reason)
	}
	return "", fmt.Errorf("push: APNs send failed (status %d): %s", res.StatusCode, res.Reason)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// IsUnregistered reports whether err marks a permanently dead token.
func IsUnregistered(err error) bool {
	return errors.Is(err, ErrTokenUnregistered)
}
