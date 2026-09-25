package notification

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DeviceType mirrors the notification_device_type_option Postgres enum
// (ios | android | iosvoip).
type DeviceType string

const (
	DeviceIOS     DeviceType = "ios"
	DeviceAndroid DeviceType = "android"
	DeviceIOSVoIP DeviceType = "iosvoip"
)

// ParseDeviceType validates a device type string.
func ParseDeviceType(s string) (DeviceType, error) {
	switch DeviceType(s) {
	case DeviceIOS, DeviceAndroid, DeviceIOSVoIP:
		return DeviceType(s), nil
	default:
		return "", fmt.Errorf("invalid device type %q", s)
	}
}

// State mirrors the notification_state Postgres enum (unseen | seen | done).
type State string

const (
	StateUnseen State = "unseen"
	StateSeen   State = "seen"
	StateDone   State = "done"
)

// ActiveStates are the states included in the default notification list
// (matches Rust NotificationState::ACTIVE).
var ActiveStates = []State{StateUnseen, StateSeen}

// Entity identifies the thing a notification is about
// (model_entity::Entity — serializes as {entity_type, entity_id}).
type Entity struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
}

// TaggedContent is the adjacently-tagged notification payload stored in
// notification.metadata alongside notification_event_type.
type TaggedContent struct {
	Tag     string          `json:"tag"`
	Content json.RawMessage `json:"content"`
}

// Request mirrors Rust SendNotificationRequestBuilder<TaggedContent<T>>.
// Option fields are always serialized (as null) to stay wire-compatible
// with the Rust serde format.
type Request struct {
	NotificationEntity          Entity        `json:"notification_entity"`
	SecondaryNotificationEntity *Entity       `json:"secondary_notification_entity"`
	Notification                TaggedContent `json:"notification"`
	SenderID                    *string       `json:"sender_id"`
	RecipientIDs                []string      `json:"recipient_ids"`
}

// MessageAttributes mirrors Rust mobile::MessageAttributes.
type MessageAttributes struct {
	PushType    string `json:"push_type"` // "Alert" | "Background" (Rust variant names) or lowercase
	CollapseKey string `json:"collapse_key"`
}

// NormalizedPushType returns the lowercase push type.
func (m MessageAttributes) NormalizedPushType() string {
	switch m.PushType {
	case "Alert", "alert":
		return "alert"
	case "Background", "background":
		return "background"
	case "Voip", "voip":
		return "voip"
	default:
		return m.PushType
	}
}

// APNSPushNotification mirrors Rust apple::APNSPushNotification<T>:
// {"aps": {...}, ...flattened custom data}.
type APNSPushNotification struct {
	Aps  map[string]any `json:"aps"`
	Data map[string]any `json:"-"`
}

// MarshalJSON merges aps + flattened data into one object.
func (n APNSPushNotification) MarshalJSON() ([]byte, error) {
	out := map[string]any{"aps": n.Aps}
	for k, v := range n.Data {
		out[k] = v
	}
	return json.Marshal(out)
}

// UnmarshalJSON splits the object back into aps + remaining data keys.
func (n *APNSPushNotification) UnmarshalJSON(raw []byte) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	n.Data = map[string]any{}
	if aps, ok := m["aps"].(map[string]any); ok {
		n.Aps = aps
		delete(m, "aps")
	}
	for k, v := range m {
		n.Data[k] = v
	}
	return nil
}

// AlertText extracts title and body from an aps dict for push delivery.
func (n APNSPushNotification) AlertText() (title, body string) {
	alert, ok := n.Aps["alert"]
	if !ok {
		return "", ""
	}
	switch a := alert.(type) {
	case string:
		return "", a
	case map[string]any:
		if t, ok := a["title"].(string); ok {
			title = t
		}
		if b, ok := a["body"].(string); ok {
			body = b
		}
	}
	return title, body
}

// BuildApnsOutput mirrors Rust request::BuildApnsOutput.
type BuildApnsOutput struct {
	Notif APNSPushNotification `json:"notif"`
	Attr  MessageAttributes    `json:"attr"`
}

// EmailCreateBundle mirrors Rust queue_message::EmailCreateBundle.
type EmailCreateBundle struct {
	Content         EmailContent    `json:"content"`
	RateLimitConfig json.RawMessage `json:"rate_limit_config,omitempty"`
	RateLimitKey    json.RawMessage `json:"rate_limit_key,omitempty"`
}

// EmailContent is subject + body for an email notification.
type EmailContent struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// SendRequest mirrors Rust SendNotificationRequest<Value, Value> as carried
// on the ingress queue (field names are the wire contract).
type SendRequest struct {
	Req             Request            `json:"req"`
	UUIDToWrite     uuid.UUID          `json:"uuid_to_write"`
	BuildApns       *BuildApnsOutput   `json:"build_apns"`
	BuildEmail      *EmailCreateBundle `json:"build_email"`
	SendConnGateway bool               `json:"send_conn_gateway"`
}

// IngressMessage mirrors Rust queue_message::IngressQueueMessage.
type IngressMessage struct {
	Request SendRequest `json:"request"`
}

// DeliveryMessage is the per-user work item published to
// notifications.delivery. Go-native format (the Rust per-channel QueueMessage
// fanout is collapsed into per-user messages).
type DeliveryMessage struct {
	NotificationID uuid.UUID `json:"notification_id"`
	UserID         string    `json:"user_id"`
	// Row is the realtime/digest payload (UserNotificationRow shape).
	Row Row `json:"row"`
	// Push carries the APNs payload + attributes when the producer requested
	// push delivery (Rust build_apns).
	Push *PushSpec `json:"push,omitempty"`
	// Email carries rendered email content for direct email delivery.
	Email *EmailContent `json:"email,omitempty"`
	// Realtime indicates a connection-gateway send was requested
	// (Rust send_conn_gateway). Default true when Push/Email are nil.
	Realtime bool `json:"realtime"`
	// DigestDeferred is the egress continuation of the Rust digest state
	// machine: ingress decided "push enabled → Indeterminate", so delivery
	// batches the notification for digest email iff every push endpoint fails
	// (StateMachineDriverB). Only ever set when Push is non-nil.
	DigestDeferred bool `json:"digest_deferred,omitempty"`
}

// PushSpec is the provider-agnostic push payload for one user.
type PushSpec struct {
	PushType    string `json:"push_type"` // "alert" | "background" | "voip"
	CollapseKey string `json:"collapse_key"`
	// APS is the verbatim `aps` dictionary the producer supplied
	// (APNSPushNotification.aps); APNs sends it as-is per platform. Never
	// forwarded to FCM.
	APS map[string]any `json:"aps,omitempty"`
	// Data is the producer's custom payload — flattened into the APNs root
	// object and sent as the FCM `data` map.
	Data map[string]any `json:"data,omitempty"`
	// Payload is the legacy merged {"aps":…, …data} map written by earlier
	// versions; split on the "aps" key when APS/Data are unset.
	Payload map[string]any `json:"payload,omitempty"`
	Title   string         `json:"title,omitempty"`
	Body    string         `json:"body,omitempty"`
}

// resolve splits the spec into the verbatim aps dict and custom data,
// tolerating the legacy merged Payload form.
func (p *PushSpec) resolve() (aps, data map[string]any) {
	aps = p.APS
	data = p.Data
	for k, v := range p.Payload {
		if k == "aps" {
			if aps == nil {
				if m, ok := v.(map[string]any); ok {
					aps = m
				}
			}
			continue
		}
		if data == nil {
			data = map[string]any{}
		}
		data[k] = v
	}
	return aps, data
}

// Row is the user_notification + notification join row
// (Rust UserNotificationRow<Value>).
type Row struct {
	OwnerID               string    `json:"owner_id"`
	NotificationID        uuid.UUID `json:"id"`
	NotificationEventType string    `json:"notification_event_type"`
	Entity
	Sent                 bool            `json:"sent"`
	State                State           `json:"state"`
	CreatedAt            time.Time       `json:"created_at"`
	ViewedAt             *time.Time      `json:"viewed_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
	DeletedAt            *time.Time      `json:"deleted_at"`
	NotificationMetadata json.RawMessage `json:"notification_metadata"`
	SenderID             *string         `json:"sender_id"`
}

// DisabledNotificationType mirrors Rust DisabledNotificationType.
type DisabledNotificationType struct {
	UserID                string `json:"user_id"`
	NotificationEventType string `json:"notification_event_type"`
}

// DeviceEndpoint is a registered device (raw provider token).
type DeviceEndpoint struct {
	Type  DeviceType
	Token string
}

// Push event types (Rust PushNotificationEventType on SNS events).
const (
	PushEventDeliverySuccess = "DeliverySuccess"
	PushEventDeliveryFailure = "DeliveryFailure"
	PushEventEndpointDeleted = "EndpointDeleted"
)

// PushEvent mirrors the Rust SNS push-notification platform event
// (delivery failure / endpoint deleted) carried on notifications.push_events.
type PushEvent struct {
	Token      string `json:"token"`
	Endpoint   string `json:"endpoint_arn,omitempty"` // legacy SNS field; treated as token
	DeviceType string `json:"device_type,omitempty"`  // scopes endpoint deletion (Rust's arn was per-platform)
	EventType  string `json:"event_type"`             // DeliveryFailure | EndpointDeleted
	MessageID  string `json:"message_id,omitempty"`
}

// Cursor is the base64-JSON page cursor compatible with
// models_pagination::Cursor<Uuid, CursorVal<CreatedAt>, ()>:
// {"id": uuid, "limit": n, "val": {"sort_type": null, "last_val": ts}, "filter": null}
type cursor struct {
	ID    uuid.UUID `json:"id"`
	Limit int       `json:"limit"`
	Val   struct {
		SortType *string   `json:"sort_type"`
		LastVal  time.Time `json:"last_val"`
	} `json:"val"`
	Filter json.RawMessage `json:"filter"`
}

// encodeCursor produces the base64-encoded next-page cursor.
func encodeCursor(id uuid.UUID, limit int, lastVal time.Time) (string, error) {
	var c cursor
	c.ID = id
	c.Limit = limit
	c.Val.LastVal = lastVal.UTC()
	c.Filter = json.RawMessage("null")
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// decodeCursor parses a base64 cursor. Returns nil for empty input.
func decodeCursor(s string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("failed to decode cursor value")
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("the cursor contained unexpected data")
	}
	return &c, nil
}

// emailPart extracts the email portion of a `macro|<email>` user id.
func emailPart(userID string) string {
	const prefix = "macro|"
	if len(userID) > len(prefix) && userID[:len(prefix)] == prefix {
		return userID[len(prefix):]
	}
	return userID
}
