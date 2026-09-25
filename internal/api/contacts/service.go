package contacts

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// Notifier is the ContactsNotifier port: invalidate cached contacts for
// users. The Rust impl POSTed to connection_gateway; the self-host port
// publishes a realtime subject that `macro gateway` fans out to websockets.
type Notifier interface {
	InvalidateContactsForUsers(ctx context.Context, userIDs []string) error
}

// NATSNotifier publishes {"message_type":"contacts_invalidation"} on
// realtime.user.<user_id> (core NATS, no persistence).
type NATSNotifier struct {
	NC *nats.Conn
}

func (n *NATSNotifier) InvalidateContactsForUsers(ctx context.Context, userIDs []string) error {
	payload, _ := json.Marshal(map[string]any{
		"message_type": "contacts_invalidation",
		"message":      map[string]any{},
	})
	for _, uid := range userIDs {
		if err := n.NC.Publish("realtime.user."+uid, payload); err != nil {
			slog.Error("contacts: failed to invalidate contacts", "user_id", uid, "err", err)
		}
	}
	return nil
}

// NoopNotifier for environments without NATS.
type NoopNotifier struct{}

func (NoopNotifier) InvalidateContactsForUsers(context.Context, []string) error { return nil }

// ContactsMessage mirrors the untagged ContactsMessage SQS payload:
// either {"users":[...]} (complete graph) or {"connections":[{first,second}]}.
type contactsNodes struct {
	Users []string `json:"users"`
}

type contactConnection struct {
	First  string `json:"first"`
	Second string `json:"second"`
}

type contactConnections struct {
	Connections []contactConnection `json:"connections"`
}

// ParseMessage parses a queue body into a contacts message. Returns nil when
// the body matches neither variant (untagged serde semantics).
func ParseMessage(body []byte) any {
	var nodes contactsNodes
	if err := json.Unmarshal(body, &nodes); err == nil && nodes.Users != nil {
		return nodes
	}
	var conns contactConnections
	if err := json.Unmarshal(body, &conns); err == nil && conns.Connections != nil {
		return conns
	}
	return nil
}

// Service is the contacts domain service (ContactsDomainService).
type Service struct {
	Repo     Repository
	Notifier Notifier
}

// QueryContacts returns the user's contacts plus the user themself (the
// graph has no self-edge; Rust inserts it artificially).
func (s *Service) QueryContacts(ctx context.Context, userID string) ([]string, error) {
	res, err := s.Repo.GetContacts(ctx, userID)
	if err != nil {
		return nil, err
	}
	return append(res, userID), nil
}

// AddContactNodes connects all users pairwise (complete graph) then notifies.
func (s *Service) AddContactNodes(ctx context.Context, users []string) error {
	var conns [][2]string
	seen := map[string]struct{}{}
	var uniq []string
	for _, u := range users {
		if _, ok := seen[u]; !ok {
			seen[u] = struct{}{}
			uniq = append(uniq, u)
		}
	}
	for i := 0; i < len(uniq); i++ {
		for j := i + 1; j < len(uniq); j++ {
			conns = append(conns, [2]string{uniq[i], uniq[j]})
		}
	}
	if err := s.Repo.CreateConnections(ctx, conns); err != nil {
		return err
	}
	return s.Notifier.InvalidateContactsForUsers(ctx, uniq)
}

// AddContactConnections upserts explicit relationships then notifies.
func (s *Service) AddContactConnections(ctx context.Context, connections []contactConnection) error {
	var conns [][2]string
	userSet := map[string]struct{}{}
	for _, c := range connections {
		conns = append(conns, [2]string{c.First, c.Second})
		userSet[c.First] = struct{}{}
		userSet[c.Second] = struct{}{}
	}
	if err := s.Repo.CreateConnections(ctx, conns); err != nil {
		return err
	}
	users := make([]string, 0, len(userSet))
	for u := range userSet {
		users = append(users, u)
	}
	return s.Notifier.InvalidateContactsForUsers(ctx, users)
}

// ProcessMessage applies a queue message (nodes or connections).
func (s *Service) ProcessMessage(ctx context.Context, msg any) error {
	switch m := msg.(type) {
	case contactsNodes:
		return s.AddContactNodes(ctx, m.Users)
	case contactConnections:
		return s.AddContactConnections(ctx, m.Connections)
	default:
		return nil
	}
}

// PollOutbox applies unapplied backfill rows (ContactsOutboxServiceImpl).
func (s *Service) PollOutbox(ctx context.Context) error {
	msgs, err := s.Repo.GetUnappliedOutbox(ctx)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if err := s.AddContactNodes(ctx, m.ChannelParticipants); err != nil {
			continue
		}
		_ = s.Repo.MarkOutboxApplied(ctx, m.ID)
	}
	return nil
}

// RunOutboxWorker ports OutboxWorker::run: poll every 1s, 15s per-poll
// timeout, forever.
func (s *Service) RunOutboxWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := s.PollOutbox(pctx); err != nil {
			slog.Error("contacts: failed to poll outbox", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
