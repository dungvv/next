package agentharness

import (
	"encoding/json"
	"testing"
)

func TestMentionedBotIDs(t *testing.T) {
	var post postedMessage
	post.Mentions = []struct {
		EntityType string `json:"entityType"`
		EntityID   string `json:"entityId"`
	}{
		{EntityType: "bot", EntityID: "bot|" + MacroCoderBotID},
		{EntityType: "bot", EntityID: "bot|" + MacroCoderBotID}, // dup
		{EntityType: "user", EntityID: "macro|a@b.com"},
		{EntityType: "bot", EntityID: "bot|" + MacroAIBotID}, // excluded: no sessions
		{EntityType: "document", EntityID: "doc-1"},
	}
	got := mentionedBotIDs(post)
	if len(got) != 1 || got[0] != MacroCoderBotID {
		t.Fatalf("got %v", got)
	}
}

func TestControlRequestUnmarshal(t *testing.T) {
	var req ControlRequest
	err := json.Unmarshal([]byte(
		`{"actionId":"a1","type":"prompt","prompt":"hello"}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	if req.ActionID == nil || *req.ActionID != "a1" {
		t.Fatalf("actionId = %v", req.ActionID)
	}
	if req.Action["type"] != "prompt" || req.Action["prompt"] != "hello" {
		t.Fatalf("action = %v", req.Action)
	}
	if _, leaked := req.Action["actionId"]; leaked {
		t.Fatal("actionId leaked into action")
	}
}

func TestQueueStore(t *testing.T) {
	q := newQueueStore()
	q.add("s1", QueuedAction{ActionID: "a1", Action: map[string]any{"type": "prompt", "prompt": "x"}})
	q.add("s1", QueuedAction{ActionID: "a2"})
	if got := q.list("s1"); len(got) != 2 || got[0].ActionID != "a1" {
		t.Fatalf("list = %v", got)
	}
	if !q.edit("s1", "a1", "new") {
		t.Fatal("edit failed")
	}
	if got := q.list("s1"); got[0].Action["prompt"] != "new" {
		t.Fatalf("after edit = %v", got)
	}
	if !q.remove("s1", "a2") || q.remove("s1", "a2") {
		t.Fatal("remove semantics wrong")
	}
}

func TestSandboxSizeLimits(t *testing.T) {
	for _, s := range []SandboxSize{SandboxSmall, SandboxDefault, SandboxLarge} {
		if !s.valid() || s.limits().NanoCPUs <= 0 || s.limits().MemoryBytes <= 0 {
			t.Fatalf("bad tier %s", s)
		}
	}
	if SandboxSize("huge").valid() {
		t.Fatal("invalid size accepted")
	}
}
