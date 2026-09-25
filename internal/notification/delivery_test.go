package notification

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/macro-inc/macro/pkg/mail"
	"github.com/macro-inc/macro/pkg/push"
)

// ---- fakes ----

type fakeRepo struct {
	Repository // embedded so unimplemented methods panic only if called

	devices   map[string][]DeviceEndpoint
	deleted   []string // endpoints passed to DeleteDeviceByEndpoint
	receipts  []string // recorded message ids
	sentCalls []uuid.UUID
}

func (f *fakeRepo) GetDeviceEndpoints(_ context.Context, userIDs []string) (map[string][]DeviceEndpoint, error) {
	out := map[string][]DeviceEndpoint{}
	for _, u := range userIDs {
		out[u] = f.devices[u]
	}
	return out, nil
}

func (f *fakeRepo) DeleteDeviceByEndpoint(_ context.Context, endpoint string) error {
	f.deleted = append(f.deleted, endpoint)
	return nil
}

func (f *fakeRepo) RecordMessageReceipt(_ context.Context, messageID, _ string, _ uuid.UUID) error {
	f.receipts = append(f.receipts, messageID)
	return nil
}

func (f *fakeRepo) UpdateSentStatus(_ context.Context, id uuid.UUID, _ []string) error {
	f.sentCalls = append(f.sentCalls, id)
	return nil
}

type fakePush struct {
	mu   sync.Mutex
	sent []push.Message
	// fail maps token → error (use push.ErrTokenUnwrapped style errors)
	fail map[string]error
}

func (f *fakePush) Send(_ context.Context, m push.Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	if err := f.fail[m.Token]; err != nil {
		return "", err
	}
	return "msg-" + m.Token, nil
}

type fakeMail struct {
	mu   sync.Mutex
	sent []mail.Message
	err  error
}

func (f *fakeMail) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return f.err
}

type fakeBatcher struct {
	mu   sync.Mutex
	rows []Row
}

func (f *fakeBatcher) Add(_ context.Context, r Row, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, r)
	return nil
}

func (f *fakeBatcher) ClaimReady(_ context.Context) (ClaimResult, error) {
	return ClaimResult{Kind: ClaimEmpty}, nil
}

// gatewayServer returns a *GatewayClient pointed at a test server that
// reports the given online users.
func gatewayServer(t *testing.T, online []string) (*GatewayClient, *struct{ payload []byte }) {
	t.Helper()
	var last struct{ payload []byte }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req gatewaySendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		last.payload, _ = json.Marshal(req.Payload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"online_users": online})
	}))
	t.Cleanup(srv.Close)
	return NewGatewayClient(srv.URL, ""), &last
}

func testRow(userID string) Row {
	return Row{
		OwnerID:               userID,
		NotificationID:        uuid.New(),
		NotificationEventType: "channel_mention",
		Entity:                Entity{EntityType: "channel", EntityID: "chan-1"},
		State:                 StateUnseen,
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
		NotificationMetadata:  json.RawMessage(`{"senderName":"a"}`),
	}
}

func testMsg(row Row) DeliveryMessage {
	return DeliveryMessage{
		NotificationID: row.NotificationID,
		UserID:         row.OwnerID,
		Row:            row,
		Realtime:       true,
		Push:           &PushSpec{PushType: "alert", APS: map[string]any{"alert": map[string]any{"title": "hi"}}},
	}
}

// ---- finding 1: online users still get explicit email ----

func TestDeliver_OnlineUserStillGetsEmail(t *testing.T) {
	row := testRow("macro|u@x.com")
	msg := testMsg(row)
	msg.Email = &EmailContent{Subject: "hi", Body: "b"}

	repo := &fakeRepo{devices: map[string][]DeviceEndpoint{}}
	pushes := &fakePush{fail: map[string]error{}}
	mailer := &fakeMail{}
	gw, _ := gatewayServer(t, []string{row.OwnerID})
	d := &Delivery{repo: repo, gateway: gw, pushes: &push.Sender{APNs: pushes}, mail: mailer, mailFrom: "n@x.com", window: time.Minute}

	if err := d.Deliver(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("expected explicit email to be sent to online user, got %d sends", len(mailer.sent))
	}
	if mailer.sent[0].To != "u@x.com" {
		t.Fatalf("unexpected email recipient %q", mailer.sent[0].To)
	}
	if len(pushes.sent) != 0 {
		t.Fatalf("online user must not receive push")
	}
	if len(repo.sentCalls) != 1 {
		t.Fatalf("expected sent status update")
	}
}

func TestDeliver_OnlineNoEmailSkipsPush(t *testing.T) {
	row := testRow("macro|u@x.com")
	msg := testMsg(row)

	repo := &fakeRepo{devices: map[string][]DeviceEndpoint{row.OwnerID: {{Type: DeviceIOS, Token: "tok1"}}}}
	pushes := &fakePush{fail: map[string]error{}}
	gw, _ := gatewayServer(t, []string{row.OwnerID})
	d := &Delivery{repo: repo, gateway: gw, pushes: &push.Sender{APNs: pushes}, window: time.Minute}

	if err := d.Deliver(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(pushes.sent) != 0 {
		t.Fatal("online user must not be pushed")
	}
}

// ---- findings 2/5: voip routing and payload split ----

func TestEndpointTargetRouting(t *testing.T) {
	cases := []struct {
		epType   DeviceType
		pushType string
		platform push.Platform
		wantPT   push.PushType
		ok       bool
	}{
		{DeviceIOS, "alert", push.PlatformIOS, push.PushTypeAlert, true},
		{DeviceIOS, "background", push.PlatformIOS, push.PushTypeBackground, true},
		{DeviceIOS, "voip", "", "", false}, // voip must not hit regular ios endpoint
		{DeviceAndroid, "alert", push.PlatformAndroid, push.PushTypeAlert, true},
		{DeviceAndroid, "voip", "", "", false}, // android has no voip
		{DeviceIOSVoIP, "voip", push.PlatformIOSVoIP, push.PushTypeVoIP, true},
		{DeviceIOSVoIP, "alert", "", "", false}, // voip endpoint only takes voip pushes
	}
	for _, c := range cases {
		p, pt, ok := endpointTarget(DeviceEndpoint{Type: c.epType, Token: "t"}, c.pushType)
		if ok != c.ok || p != c.platform || pt != c.wantPT {
			t.Fatalf("%s/%s → (%v,%v,%v), want (%v,%v,%v)", c.epType, c.pushType, p, pt, ok, c.platform, c.wantPT, c.ok)
		}
	}
}

func TestDeliver_VoipFailureDoesNotDeleteMainRegistration(t *testing.T) {
	row := testRow("macro|u@x.com")
	msg := testMsg(row)
	msg.Push.PushType = "voip"

	repo := &fakeRepo{devices: map[string][]DeviceEndpoint{row.OwnerID: {
		{Type: DeviceIOS, Token: "shared-tok"},
		{Type: DeviceIOSVoIP, Token: "shared-tok"},
	}}}
	pushes := &fakePush{fail: map[string]error{"shared-tok": push.ErrTokenUnregistered}}
	gw, _ := gatewayServer(t, nil)
	d := &Delivery{repo: repo, gateway: gw, pushes: &push.Sender{APNs: pushes}, digests: &fakeBatcher{}, window: time.Minute}

	if err := d.Deliver(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	// Only the voip endpoint received the push attempt.
	if len(pushes.sent) != 1 || pushes.sent[0].PushType != push.PushTypeVoIP {
		t.Fatalf("expected exactly one voip push, got %+v", pushes.sent)
	}
	// The deleted endpoint is the type-namespaced voip endpoint — the ios
	// registration (endpoint "ios:shared-tok") is untouched.
	if len(repo.deleted) != 1 || repo.deleted[0] != "iosvoip:shared-tok" {
		t.Fatalf("deleted endpoints: %v", repo.deleted)
	}
}

func TestDeliver_APSNotSentToAndroid(t *testing.T) {
	row := testRow("macro|u@x.com")
	msg := testMsg(row)
	msg.Push.APS = map[string]any{"alert": map[string]any{"title": "T"}, "badge": 2}
	msg.Push.Data = map[string]any{"deep": "link"}

	repo := &fakeRepo{devices: map[string][]DeviceEndpoint{row.OwnerID: {{Type: DeviceAndroid, Token: "fcm-tok"}}}}
	fcm := &fakePush{fail: map[string]error{}}
	gw, _ := gatewayServer(t, nil)
	d := &Delivery{repo: repo, gateway: gw, pushes: &push.Sender{FCM: fcm}, window: time.Minute}

	if err := d.Deliver(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(fcm.sent) != 1 {
		t.Fatalf("expected one fcm send, got %d", len(fcm.sent))
	}
	m := fcm.sent[0]
	if m.Data["deep"] != "link" {
		t.Fatalf("custom data lost: %+v", m.Data)
	}
	if _, bad := m.Data["aps"]; bad {
		t.Fatal("aps key must not leak into FCM data")
	}
	if len(repo.receipts) != 1 || repo.receipts[0] != "msg-fcm-tok" {
		t.Fatalf("receipt not recorded: %v", repo.receipts)
	}
}

func TestDeliver_IOSGetsVerbatimAPS(t *testing.T) {
	row := testRow("macro|u@x.com")
	msg := testMsg(row)
	msg.Push.APS = map[string]any{"alert": map[string]any{"title": "T"}, "sound": "bing"}
	msg.Push.Data = map[string]any{"k": "v"}

	repo := &fakeRepo{devices: map[string][]DeviceEndpoint{row.OwnerID: {{Type: DeviceIOS, Token: "ios-tok"}}}}
	apns := &fakePush{fail: map[string]error{}}
	gw, _ := gatewayServer(t, nil)
	d := &Delivery{repo: repo, gateway: gw, pushes: &push.Sender{APNs: apns}, window: time.Minute}

	if err := d.Deliver(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(apns.sent) != 1 {
		t.Fatalf("expected one apns send")
	}
	m := apns.sent[0]
	if m.APS["sound"] != "bing" {
		t.Fatalf("verbatim aps lost: %+v", m.APS)
	}
	if m.Data["k"] != "v" {
		t.Fatalf("data lost: %+v", m.Data)
	}
}

// ---- finding 3: digest deferral ----

func TestDeliver_DigestOnlyWhenDeferredAndAllFail(t *testing.T) {
	row := testRow("macro|u@x.com")

	t.Run("deferred and push fails → queued", func(t *testing.T) {
		msg := testMsg(row)
		msg.DigestDeferred = true
		repo := &fakeRepo{devices: map[string][]DeviceEndpoint{row.OwnerID: {{Type: DeviceIOS, Token: "t"}}}}
		apns := &fakePush{fail: map[string]error{"t": errors.New("boom")}}
		batcher := &fakeBatcher{}
		gw, _ := gatewayServer(t, nil)
		d := &Delivery{repo: repo, gateway: gw, pushes: &push.Sender{APNs: apns}, digests: batcher, window: time.Minute}
		if err := d.Deliver(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
		if len(batcher.rows) != 1 {
			t.Fatalf("expected digest batch, got %d", len(batcher.rows))
		}
	})

	t.Run("not deferred and push fails → dropped", func(t *testing.T) {
		msg := testMsg(row)
		repo := &fakeRepo{devices: map[string][]DeviceEndpoint{row.OwnerID: {{Type: DeviceIOS, Token: "t"}}}}
		apns := &fakePush{fail: map[string]error{"t": errors.New("boom")}}
		batcher := &fakeBatcher{}
		gw, _ := gatewayServer(t, nil)
		d := &Delivery{repo: repo, gateway: gw, pushes: &push.Sender{APNs: apns}, digests: batcher, window: time.Minute}
		if err := d.Deliver(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
		if len(batcher.rows) != 0 {
			t.Fatal("non-deferred push failure must not digest")
		}
	})

	t.Run("deferred and push succeeds → no digest", func(t *testing.T) {
		msg := testMsg(row)
		msg.DigestDeferred = true
		repo := &fakeRepo{devices: map[string][]DeviceEndpoint{row.OwnerID: {{Type: DeviceIOS, Token: "t"}}}}
		apns := &fakePush{fail: map[string]error{}}
		batcher := &fakeBatcher{}
		gw, _ := gatewayServer(t, nil)
		d := &Delivery{repo: repo, gateway: gw, pushes: &push.Sender{APNs: apns}, digests: batcher, window: time.Minute}
		if err := d.Deliver(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
		if len(batcher.rows) != 0 {
			t.Fatal("successful push must not digest")
		}
	})
}

// ---- finding 7: realtime wire shape ----

func TestDeliver_RealtimePayloadShape(t *testing.T) {
	row := testRow("macro|u@x.com")
	row.ViewedAt = nil
	msg := testMsg(row)

	gw, last := gatewayServer(t, []string{row.OwnerID})
	d := &Delivery{repo: &fakeRepo{devices: map[string][]DeviceEndpoint{}}, gateway: gw, window: time.Minute}
	if err := d.Deliver(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(last.payload, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["notification_id"] != row.NotificationID.String() {
		t.Fatalf("missing notification_id: %v", wire)
	}
	if _, hasID := wire["id"]; hasID {
		t.Fatal("RealtimeNotif must not carry an `id` field")
	}
	if _, hasOwner := wire["owner_id"]; hasOwner {
		t.Fatal("RealtimeNotif must not carry owner_id")
	}
	if wire["sent"] != true {
		t.Fatal("RealtimeNotif.sent is always true")
	}
	meta, _ := wire["notification_metadata"].(map[string]any)
	if meta["tag"] != "channel_mention" {
		t.Fatalf("metadata tag wrong: %v", meta)
	}
	if wire["entity_type"] != "channel" || wire["entity_id"] != "chan-1" {
		t.Fatalf("entity not flattened: %v", wire)
	}
}

// ---- finding 3: digest gate ----

type stubUsers map[string]bool

func (s stubUsers) ExistingUsers(_ context.Context, emails []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, e := range emails {
		out[e] = s[e]
	}
	return out, nil
}

type stubOnline map[string]time.Duration

func (s stubOnline) LastOnlineAges(_ context.Context, ids []string) (map[string]time.Duration, error) {
	out := map[string]time.Duration{}
	for _, id := range ids {
		out[id] = s[id]
	}
	return out, nil
}

type gateRepo struct {
	Repository
	muted     map[string]bool
	endpoints map[string][]DeviceEndpoint
}

func (r gateRepo) GetMutedUsers(_ context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = r.muted[id]
	}
	return out, nil
}

func (r gateRepo) GetDeviceEndpoints(_ context.Context, ids []string) (map[string][]DeviceEndpoint, error) {
	out := map[string][]DeviceEndpoint{}
	for _, id := range ids {
		out[id] = r.endpoints[id]
	}
	return out, nil
}

func TestDigestGateDecisions(t *testing.T) {
	row := func(user, evType string) Row {
		r := testRow(user)
		r.NotificationEventType = evType
		return r
	}
	newGate := func(b *fakeBatcher) digestGatePorts {
		return digestGatePorts{
			repo: gateRepo{
				muted: map[string]bool{"macro|muted": true},
				endpoints: map[string][]DeviceEndpoint{
					"macro|ios":    {{Type: DeviceIOS, Token: "t"}},
					"macro|and":    {{Type: DeviceAndroid, Token: "t"}},
					"macro|voip":   {{Type: DeviceIOSVoIP, Token: "t"}},
					"macro|noacce": {{Type: DeviceIOS, Token: "t"}},
				},
			},
			users:     stubUsers{"noacce": false, "ios": true, "and": true, "voip": true, "off": true, "muted": true, "recent": true},
			online:    stubOnline{"off": 2 * time.Hour, "recent": time.Minute},
			digests:   b,
			window:    24 * time.Hour,
			threshold: time.Hour,
		}
	}

	t.Run("blocklisted type → dont send", func(t *testing.T) {
		g := newGate(&fakeBatcher{})
		got := g.decide(row("macro|ios", "new_email"), map[string]bool{},
			map[string][]DeviceEndpoint{"macro|ios": {{Type: DeviceIOS, Token: "t"}}},
			map[string]bool{"ios": true}, nil)
		if got != digestDontSend {
			t.Fatal("blocklisted type must not digest")
		}
	})

	t.Run("no account → dont send", func(t *testing.T) {
		g := newGate(&fakeBatcher{})
		got := g.decide(row("macro|noacce", "channel_mention"), map[string]bool{},
			map[string][]DeviceEndpoint{"macro|noacce": {{Type: DeviceIOS, Token: "t"}}},
			map[string]bool{"noacce": false}, nil)
		if got != digestDontSend {
			t.Fatal("user without account must not digest")
		}
	})

	t.Run("ios endpoint → deferred", func(t *testing.T) {
		g := newGate(&fakeBatcher{})
		got := g.decide(row("macro|ios", "channel_mention"), map[string]bool{},
			map[string][]DeviceEndpoint{"macro|ios": {{Type: DeviceIOS, Token: "t"}}},
			map[string]bool{"ios": true}, nil)
		if got != digestDeferred {
			t.Fatal("ios user should defer to push-failure path")
		}
	})

	t.Run("android only → dont send (no iOS message to defer on)", func(t *testing.T) {
		g := newGate(&fakeBatcher{})
		got := g.decide(row("macro|and", "channel_mention"), map[string]bool{},
			map[string][]DeviceEndpoint{"macro|and": {{Type: DeviceAndroid, Token: "t"}}},
			map[string]bool{"and": true}, nil)
		if got != digestDontSend {
			t.Fatal("android-only users get no digest_state (Rust attaches it to the iOS message only)")
		}
	})

	t.Run("push disabled + offline long → batch now", func(t *testing.T) {
		b := &fakeBatcher{}
		g := newGate(b)
		got := g.decide(row("macro|off", "channel_mention"), map[string]bool{},
			map[string][]DeviceEndpoint{},
			map[string]bool{"off": true},
			map[string]time.Duration{"macro|off": 2 * time.Hour})
		if got != digestBatchNow {
			t.Fatal("offline push-disabled user should batch immediately")
		}
	})

	t.Run("push disabled + recently online → dont send", func(t *testing.T) {
		g := newGate(&fakeBatcher{})
		got := g.decide(row("macro|recent", "channel_mention"), map[string]bool{},
			map[string][]DeviceEndpoint{},
			map[string]bool{"recent": true},
			map[string]time.Duration{"macro|recent": time.Minute})
		if got != digestDontSend {
			t.Fatal("recently-online push-disabled user must not digest")
		}
	})

	t.Run("muted counts as push disabled", func(t *testing.T) {
		g := newGate(&fakeBatcher{})
		got := g.decide(row("macro|muted", "channel_mention"), map[string]bool{"macro|muted": true},
			map[string][]DeviceEndpoint{"macro|muted": {{Type: DeviceIOS, Token: "t"}}},
			map[string]bool{"muted": true},
			map[string]time.Duration{"macro|muted": 2 * time.Hour})
		if got != digestBatchNow {
			t.Fatal("muted user with endpoints has push disabled → batch when offline long")
		}
	})
}
