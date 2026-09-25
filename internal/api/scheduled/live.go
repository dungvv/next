package scheduled

import (
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/macro-inc/macro/pkg/events"
)

// NATSLiveUpdates publishes run-status updates onto the realtime fanout
// subject the gateway subscribes to: `realtime.user.<id>` carrying an
// events.Envelope whose type is `scheduled_action_update`.
type NATSLiveUpdates struct {
	NC *nats.Conn
}

type realtimeMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// PublishUpdate implements LiveUpdates; failures are logged and swallowed so
// UI delivery problems never mark an otherwise-successful run as failed.
func (n NATSLiveUpdates) PublishUpdate(update any) {
	var owner string
	switch u := update.(type) {
	case ScheduledActionUpdateStarted:
		owner = u.Owner
	case ScheduledActionUpdateStopped:
		owner = u.Owner
	default:
		slog.Error("scheduled: unknown update type", "update", update)
		return
	}
	payload, err := json.Marshal(update)
	if err != nil {
		slog.Error("scheduled: failed to serialize update", "err", err)
		return
	}
	env, err := events.New(ScheduledActionUpdateMessageType, "scheduled", owner, 1,
		realtimeMessage{Type: ScheduledActionUpdateMessageType, Payload: payload})
	if err != nil {
		slog.Error("scheduled: failed to build update envelope", "err", err)
		return
	}
	data, err := json.Marshal(env)
	if err != nil {
		slog.Error("scheduled: failed to serialize envelope", "err", err)
		return
	}
	if err := n.NC.Publish("realtime.user."+owner, data); err != nil {
		slog.Error("scheduled: failed to publish live update", "err", err, "owner", owner)
	}
}

// NoopLiveUpdates drops updates (used when NATS is unavailable).
type NoopLiveUpdates struct{}

func (NoopLiveUpdates) PublishUpdate(any) {}
