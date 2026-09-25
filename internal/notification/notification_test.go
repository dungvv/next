package notification

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDecodePayloadRaw(t *testing.T) {
	raw := `{"request":{"req":{"notification_entity":{"entity_type":"document","entity_id":"d1"},"secondary_notification_entity":null,"notification":{"tag":"document_mention","content":{"x":1}},"sender_id":"macro|a@b.c","recipient_ids":["macro|u@b.c"]},"uuid_to_write":"11111111-1111-1111-1111-111111111111","build_apns":null,"build_email":null,"send_conn_gateway":true}}`
	var msg IngressMessage
	if err := decodePayload([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Request.Req.RecipientIDs[0] != "macro|u@b.c" {
		t.Fatalf("unexpected recipient %v", msg.Request.Req.RecipientIDs)
	}
}

func TestDecodePayloadEnvelope(t *testing.T) {
	env := `{"id":"e1","type":"notification.send","source":"chat","subject":"notifications.ingress","time":"2024-01-01T00:00:00Z","version":1,"data":{"request":{"req":{"notification_entity":{"entity_type":"chat","entity_id":"c1"},"secondary_notification_entity":null,"notification":{"tag":"channel_mention","content":{}},"sender_id":null,"recipient_ids":["u"]},"uuid_to_write":"11111111-1111-1111-1111-111111111111","send_conn_gateway":true}}}`
	var msg IngressMessage
	if err := decodePayload([]byte(env), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Request.Req.NotificationEntity.EntityID != "c1" {
		t.Fatalf("unexpected entity %v", msg.Request.Req.NotificationEntity)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	id := uuid.New()
	ts := time.Now().UTC().Truncate(time.Microsecond)
	s, err := encodeCursor(id, 20, ts)
	if err != nil {
		t.Fatal(err)
	}
	c, err := decodeCursor(s)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || c.ID != id || c.Limit != 20 || !c.Val.LastVal.Equal(ts) {
		t.Fatalf("cursor mismatch: %+v", c)
	}
	if c, err := decodeCursor(""); err != nil || c != nil {
		t.Fatalf("empty cursor should decode to nil")
	}
	if _, err := decodeCursor("!!notbase64!!"); err == nil {
		t.Fatalf("expected error for bad cursor")
	}
}

func TestParseStates(t *testing.T) {
	// Omitted → ActiveStates
	got := parseStates(map[string][]string{})
	if len(got) != len(ActiveStates) {
		t.Fatalf("default states: %v", got)
	}
	// Empty string → no filter
	got = parseStates(map[string][]string{"states": {""}})
	if got != nil {
		t.Fatalf("empty states should disable filter: %v", got)
	}
	// Explicit list
	got = parseStates(map[string][]string{"states": {"unseen,done"}})
	if len(got) != 2 || got[0] != StateUnseen || got[1] != StateDone {
		t.Fatalf("explicit states: %v", got)
	}
}

func TestNormalizedPushType(t *testing.T) {
	for in, want := range map[string]string{
		"Alert": "alert", "alert": "alert",
		"Background": "background", "Voip": "voip", "weird": "weird",
	} {
		if got := (MessageAttributes{PushType: in}).NormalizedPushType(); got != want {
			t.Fatalf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestAlertText(t *testing.T) {
	n := APNSPushNotification{Aps: map[string]any{
		"alert": map[string]any{"title": "T", "body": "B"},
	}}
	if title, body := n.AlertText(); title != "T" || body != "B" {
		t.Fatalf("got %q/%q", title, body)
	}
	n2 := APNSPushNotification{Aps: map[string]any{"alert": "hello"}}
	if _, body := n2.AlertText(); body != "hello" {
		t.Fatalf("string alert body: %q", body)
	}
}

func TestAPNSPayloadRoundTrip(t *testing.T) {
	raw := `{"aps":{"alert":{"title":"hi"}},"deep":{"link":1}}`
	var n APNSPushNotification
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["deep"]; !ok {
		t.Fatalf("custom data lost: %s", out)
	}
	if _, ok := m["aps"]; !ok {
		t.Fatalf("aps lost: %s", out)
	}
}

func TestEmailPart(t *testing.T) {
	if got := emailPart("macro|u@x.com"); got != "u@x.com" {
		t.Fatalf("got %q", got)
	}
	if got := emailPart("u@x.com"); got != "u@x.com" {
		t.Fatalf("got %q", got)
	}
}

func TestIsBlockable(t *testing.T) {
	if !IsBlockable("channel_mention") || !IsBlockable("email-digest-notification") {
		t.Fatal("expected blockable")
	}
	if IsBlockable("invite_to_macro") || IsBlockable("") {
		t.Fatal("expected not blockable")
	}
}
