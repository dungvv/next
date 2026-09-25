package gateway

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/macro-inc/macro/pkg/events"
)

// realtimeSubject is the per-user realtime subject: realtime.user.<user_id>.
func realtimeSubject(userID string) string {
	return "realtime.user." + userID
}

// subjectUserID extracts the user id from a fanout subject. User ids are
// principals like "macro|a@b.com" and contain dots, so the id is everything
// after the known prefix — never the last token.
func subjectUserID(subject string) string {
	if id, ok := strings.CutPrefix(subject, "realtime.user."); ok {
		return id
	}
	if id, ok := strings.CutPrefix(subject, "notifications.status."); ok {
		return id
	}
	return ""
}

// subscribeFanout wires the NATS core-subject subscriptions that replace the
// Rust Redis pub/sub bridge (connection_gateway.messages) and the HTTP
// send path for cross-replica delivery: every replica subscribes, and each
// delivers to the connections it owns.
//
// A failed subscribe used to be logged once and forgotten — the replica then
// ran forever without cross-instance fanout. Retry with capped exponential
// backoff until it lands or the process shuts down.
func (s *Server) subscribeFanout(ctx context.Context, nc *nats.Conn) error {
	backoff := 250 * time.Millisecond
	const maxBackoff = 5 * time.Second
	for {
		err := func() error {
			for _, subj := range []string{events.SubjectRealtimeUser, events.SubjectNotifStatus} {
				if _, err := nc.Subscribe(subj, s.onFanout); err != nil {
					return err
				}
			}
			return nc.Flush()
		}()
		if err == nil {
			slog.Info("gateway: subscribed for realtime fanout",
				"subjects", []string{events.SubjectRealtimeUser, events.SubjectNotifStatus})
			return nil
		}
		slog.Error("gateway: fanout subscribe failed, retrying",
			"err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// onFanout delivers one NATS message to the target user's local connections.
// Bus payloads are events.Envelope (or bare {type, ...}) documents; they are
// normalized into the {type, data: <string>} wire shape the web client
// expects before hitting the socket.
func (s *Server) onFanout(msg *nats.Msg) {
	userID := subjectUserID(msg.Subject)
	if userID == "" {
		slog.Warn("gateway: fanout message with unparseable subject", "subject", msg.Subject)
		return
	}
	n := s.hub.sendToUser(userID, normalizeClientFrame(msg.Data))
	slog.Debug("gateway: fanout", "subject", msg.Subject, "user_id", userID,
		"delivered", n, "local_conns", len(s.hub.localConns(userID)))
}

// deliverRaw publishes payload to realtime.user.<id> for each user so every
// replica (including this one, via onFanout) delivers to its own sockets.
// If NATS is unavailable it falls back to local-only delivery so the send
// still lands for connections on this instance.
func (s *Server) deliverRaw(userIDs []string, payload []byte) {
	for _, userID := range userIDs {
		if userID == "" {
			continue
		}
		// Publish even while reconnecting: nats.go buffers outbound messages
		// (ReconnectBufSize) so the send still lands once the link returns.
		if s.nc != nil && !s.nc.IsClosed() {
			if err := s.nc.Publish(realtimeSubject(userID), payload); err == nil {
				continue
			} else {
				slog.Warn("gateway: nats publish failed, delivering locally",
					"user_id", userID, "err", err)
			}
		}
		s.hub.sendToUser(userID, normalizeClientFrame(payload))
	}
}
