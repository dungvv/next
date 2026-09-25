package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
)

// GatewayClient posts realtime notifications to the gateway internal API.
// Replaces the Rust ConnectionGatewayClient (which used
// {connection_gateway_url}/message/batch_send).
type GatewayClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewGatewayClient builds the gateway client.
func NewGatewayClient(baseURL, internalAPIKey string) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		apiKey:  internalAPIKey,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

type gatewaySendRequest struct {
	Users   []string `json:"users"`
	Event   string   `json:"event"`
	Payload any      `json:"payload"`
}

// Send posts {users, event, payload} to <base>/internal/send and returns the
// set of user IDs that were online (delivered over websocket).
func (g *GatewayClient) Send(ctx context.Context, users []string, event string, payload any) (map[string]bool, error) {
	if len(users) == 0 {
		return map[string]bool{}, nil
	}
	raw, err := json.Marshal(gatewaySendRequest{Users: users, Event: event, Payload: payload})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/internal/send", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("x-internal-auth-key", g.apiKey)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway send: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("gateway send: status %d: %s", resp.StatusCode, string(body))
	}

	// Accept several response spellings: {online_users}, {users}, or the Rust
	// gateway's {receipts: [{user_id, delivery_count}]}.
	var out struct {
		OnlineUsers []string `json:"online_users"`
		Users       []string `json:"users"`
		Receipts    []struct {
			UserID        string `json:"user_id"`
			DeliveryCount int    `json:"delivery_count"`
		} `json:"receipts"`
	}
	online := map[string]bool{}
	if err := json.Unmarshal(body, &out); err != nil {
		// A non-JSON 2xx is treated as "no receipts" rather than an error.
		return online, nil
	}
	for _, u := range out.OnlineUsers {
		online[u] = true
	}
	for _, u := range out.Users {
		online[u] = true
	}
	for _, rcpt := range out.Receipts {
		if rcpt.DeliveryCount > 0 {
			online[rcpt.UserID] = true
		}
	}
	return online, nil
}

// statusUpdate is the realtime payload for seen/done changes,
// mirroring Rust NotificationStatusUpdate.
type statusUpdate struct {
	Type    string        `json:"type"` // "notification_status_updated"
	Updates []statusPatch `json:"updates"`
}

type statusPatch struct {
	T string `json:"t"` // "Patch" | "Delete"
	C any    `json:"c"`
}

// StatusPublisher publishes per-user status updates on core NATS subjects
// notifications.status.<user_id> (subject catalog: SubjectNotifStatus).
type StatusPublisher struct {
	nc *nats.Conn
}

// NewStatusPublisher builds a StatusPublisher on a core NATS connection.
func NewStatusPublisher(nc *nats.Conn) *StatusPublisher {
	return &StatusPublisher{nc: nc}
}

// PublishPatches sends updated rows to notifications.status.<user_id>.
func (s *StatusPublisher) PublishPatches(userID string, rows []Row) error {
	if s == nil || s.nc == nil || len(rows) == 0 {
		return nil
	}
	updates := make([]statusPatch, 0, len(rows))
	for _, r := range rows {
		updates = append(updates, statusPatch{T: "Patch", C: r.wire()})
	}
	payload, err := json.Marshal(statusUpdate{Type: "notification_status_updated", Updates: updates})
	if err != nil {
		return err
	}
	return s.nc.Publish("notifications.status."+userID, payload)
}

// PublishDelete sends a delete update for a notification to a user.
func (s *StatusPublisher) PublishDelete(userID string, notificationID string) error {
	if s == nil || s.nc == nil {
		return nil
	}
	payload, err := json.Marshal(statusUpdate{
		Type:    "notification_status_updated",
		Updates: []statusPatch{{T: "Delete", C: map[string]string{"id": notificationID}}},
	})
	if err != nil {
		return err
	}
	return s.nc.Publish("notifications.status."+userID, payload)
}

// wire returns the row in UserNotificationRow form (the REST list/patch
// shape): the id field is "id" and owner_id is present. Metadata is wrapped
// back into its {tag, content} adjacently-tagged form.
func (r Row) wire() map[string]any {
	tagged := map[string]any{
		"tag":     r.NotificationEventType,
		"content": r.NotificationMetadata,
	}
	m := map[string]any{
		"owner_id":                r.OwnerID,
		"id":                      r.NotificationID,
		"notification_event_type": r.NotificationEventType,
		"entity_type":             r.EntityType,
		"entity_id":               r.EntityID,
		"sent":                    r.Sent,
		"state":                   r.State,
		"created_at":              r.CreatedAt,
		"viewed_at":               r.ViewedAt,
		"updated_at":              r.UpdatedAt,
		"deleted_at":              r.DeletedAt,
		"notification_metadata":   tagged,
		"sender_id":               r.SenderID,
	}
	return m
}

// realtimeWire returns the row in RealtimeNotif form — the payload the
// frontend expects inside the "notification" websocket frame's `data`
// string (crates/notification queue_message.rs::RealtimeNotif). Unlike
// wire() the id is named "notification_id" and there is no owner_id; sent
// is always true (Rust hardcodes it in ConnGatewayNotification).
func (r Row) realtimeWire() map[string]any {
	return map[string]any{
		"notification_id":         r.NotificationID,
		"notification_event_type": r.NotificationEventType,
		"entity_type":             r.EntityType,
		"entity_id":               r.EntityID,
		"sent":                    true,
		"state":                   r.State,
		"created_at":              r.CreatedAt,
		"viewed_at":               r.ViewedAt,
		"updated_at":              r.UpdatedAt,
		"deleted_at":              r.DeletedAt,
		"notification_metadata": map[string]any{
			"tag":     r.NotificationEventType,
			"content": r.NotificationMetadata,
		},
		"sender_id": r.SenderID,
	}
}
