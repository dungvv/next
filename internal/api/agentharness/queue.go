package agentharness

import (
	"sync"
)

// queueStore is the in-memory pending-action store backing the session
// control queue. The Rust control router is explicit that it is only
// mountable "in the process that owns the sessions" — queues live in memory,
// so a restart drops queued-but-unsent actions just as the Rust one does.
type queueStore struct {
	mu      sync.Mutex
	entries map[string][]QueuedAction // session id -> dispatch order
}

func newQueueStore() *queueStore {
	return &queueStore{entries: map[string][]QueuedAction{}}
}

func (q *queueStore) add(sessionID string, a QueuedAction) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entries[sessionID] = append(q.entries[sessionID], a)
}

func (q *queueStore) list(sessionID string) []QueuedAction {
	q.mu.Lock()
	defer q.mu.Unlock()
	src := q.entries[sessionID]
	out := make([]QueuedAction, len(src))
	copy(out, src)
	return out
}

func (q *queueStore) edit(sessionID, actionID, prompt string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.entries[sessionID]
	for i := range list {
		if list[i].ActionID == actionID {
			list[i].Action["type"] = "prompt"
			list[i].Action["prompt"] = prompt
			return true
		}
	}
	return false
}

func (q *queueStore) remove(sessionID, actionID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.entries[sessionID]
	for i := range list {
		if list[i].ActionID == actionID {
			q.entries[sessionID] = append(list[:i], list[i+1:]...)
			return true
		}
	}
	return false
}
