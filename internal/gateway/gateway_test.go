package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
)

// fakeBackend is an in-memory registryBackend for tests.
type fakeBackend struct {
	mu    sync.Mutex
	conns map[string]string          // connID -> userID
	ents  map[string]map[string]bool // "type|id" -> connID set
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{conns: map[string]string{}, ents: map[string]map[string]bool{}}
}

func (f *fakeBackend) name() string { return "fake" }

func (f *fakeBackend) add(_ context.Context, userID, connID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[connID] = userID
	return nil
}

func (f *fakeBackend) heartbeat(_ context.Context, userID, connID string) error {
	return f.add(context.Background(), userID, connID)
}

func (f *fakeBackend) remove(_ context.Context, _ string, connID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.conns, connID)
	for k, set := range f.ents {
		delete(set, connID)
		if len(set) == 0 {
			delete(f.ents, k)
		}
	}
	return nil
}

func (f *fakeBackend) connections(_ context.Context, userID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for c, u := range f.conns {
		if u == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeBackend) entityUsers(_ context.Context, t, id string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for c := range f.ents[t+"|"+id] {
		if u, ok := f.conns[c]; ok {
			seen[u] = true
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeBackend) trackEntity(_ context.Context, connID, userID, t, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[connID] = userID
	k := t + "|" + id
	if f.ents[k] == nil {
		f.ents[k] = map[string]bool{}
	}
	f.ents[k][connID] = true
	return nil
}

func (f *fakeBackend) untrackEntity(_ context.Context, connID, t, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ents[t+"|"+id], connID)
	return nil
}

func (f *fakeBackend) pingEntity(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeBackend) sweep(context.Context, time.Duration) (int, error) { return 0, nil }

const testSecret = "test-secret"

func testToken(t *testing.T, userID string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	s, err := tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testServer(t *testing.T) (*Server, *httptest.Server, *httptest.Server) {
	t.Helper()
	s := &Server{
		cfg:      Config{SendBuffer: 8, WriteTimeout: 5 * time.Second},
		hub:      newHub(8),
		reg:      newRegistry(newFakeBackend(), nil, 60*time.Second),
		verifier: &hmacVerifier{secret: []byte(testSecret)},
		lifeCtx:  context.Background(),
	}
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /{$}", s.handleWS)
	publicMux.HandleFunc("GET /health", s.handleHealth)
	pub := httptest.NewServer(publicMux)
	internal := httptest.NewServer(s.internalMux())
	t.Cleanup(pub.Close)
	t.Cleanup(internal.Close)
	return s, pub, internal
}

func wsURL(h *httptest.Server, token string) string {
	return "ws" + strings.TrimPrefix(h.URL, "http") + "/?token=" + token
}

func TestSubjectUserID(t *testing.T) {
	if got := subjectUserID("realtime.user.abc123"); got != "abc123" {
		t.Fatalf("got %q", got)
	}
	if got := subjectUserID("notifications.status.u-9"); got != "u-9" {
		t.Fatalf("got %q", got)
	}
	// User ids are principals containing dots — the id is everything after
	// the prefix, not the last token.
	if got := subjectUserID("realtime.user.macro|a@gmail.com"); got != "macro|a@gmail.com" {
		t.Fatalf("dotted id: got %q", got)
	}
	if got := subjectUserID("realtime.user."); got != "" {
		t.Fatalf("empty id should be %q", got)
	}
}

func TestWSAuthRejectsBadToken(t *testing.T) {
	_, pub, _ := testServer(t)
	_, resp, err := websocket.Dial(context.Background(), wsURL(pub, "garbage"), nil)
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v", resp)
	}
}

func TestWSPingPongAndSend(t *testing.T) {
	s, pub, internal := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, wsURL(pub, testToken(t, "user-1")), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	if err := ws.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("ping: %v", err)
	}
	_, pong, err := ws.Read(ctx)
	if err != nil || string(pong) != "pong" {
		t.Fatalf("pong: %v %q", err, pong)
	}

	// track_entity open registers on the entity.
	track := `{"type":"track_entity","entity_type":"document","entity_id":"doc-1","action":"open"}`
	if err := ws.Write(ctx, websocket.MessageText, []byte(track)); err != nil {
		t.Fatalf("track: %v", err)
	}
	// Wait for registration to land.
	deadline := time.Now().Add(2 * time.Second)
	for {
		users, _ := s.reg.EntityUsers(ctx, "document", "doc-1")
		if len(users) == 1 && users[0] == "user-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("entity tracking did not register")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// /internal/send delivers the envelope over the open socket.
	body := `{"users":["user-1","user-2"],"event":"doc.updated","payload":{"v":1}}`
	resp, err := http.Post(internal.URL+"/internal/send", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	defer resp.Body.Close()
	var sr sendResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if len(sr.Delivered) != 1 || sr.Delivered[0] != "user-1" {
		t.Fatalf("unexpected delivered: %+v", sr)
	}

	// The socket may first receive the user_tracking_change broadcast; read
	// until we see doc.updated.
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("bad envelope: %v", err)
		}
		if env.Type == "doc.updated" {
			break
		}
	}

	// Presence.
	resp2, err := http.Get(internal.URL + "/internal/presence?user=user-1&user=user-2")
	if err != nil {
		t.Fatal(err)
	}
	var pr struct {
		Online []string `json:"online"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if len(pr.Online) != 1 || pr.Online[0] != "user-1" {
		t.Fatalf("unexpected presence: %+v", pr)
	}

	// Rust-compat track endpoint.
	resp3, err := http.Get(internal.URL + "/track/document/doc-1")
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	if err := json.NewDecoder(resp3.Body).Decode(&users); err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if len(users) != 1 || users[0] != "user-1" {
		t.Fatalf("unexpected track users: %v", users)
	}

	ws.Close(websocket.StatusNormalClosure, "done")
	// Wait for cleanup.
	deadline = time.Now().Add(2 * time.Second)
	for s.hub.count() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.hub.count() != 0 {
		t.Fatal("connection not cleaned up")
	}
	fmt.Println("hub cleaned up")
}

func TestNormalizeClientFrame(t *testing.T) {
	// Envelope-shaped bus payload: data is hoisted and re-encoded as a
	// JSON string, matching the Rust Message { type, data: String } wire
	// shape the frontend parses.
	env := `{"id":"x","type":"doc.updated","source":"s","subject":"u","time":"t","version":1,"data":{"v":1}}`
	var frame struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(normalizeClientFrame([]byte(env)), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "doc.updated" {
		t.Fatalf("type: %q", frame.Type)
	}
	var inner map[string]int
	if err := json.Unmarshal([]byte(frame.Data), &inner); err != nil || inner["v"] != 1 {
		t.Fatalf("data must be a JSON string, got %q err=%v", frame.Data, err)
	}

	// Bare {type, ...} payloads (notification status updates) wrap the
	// whole document as the data string.
	bare := `{"type":"notification_status_updated","updates":[{"a":1}]}`
	frame = struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}{}
	if err := json.Unmarshal(normalizeClientFrame([]byte(bare)), &frame); err != nil {
		t.Fatal(err)
	}
	var wrapped map[string]any
	if err := json.Unmarshal([]byte(frame.Data), &wrapped); err != nil || wrapped["updates"] == nil {
		t.Fatalf("bare message not wrapped correctly: %q err=%v", frame.Data, err)
	}

	// data already a string is passed through verbatim.
	str := `{"type":"e","data":"{\"k\":2}"}`
	if got := string(normalizeClientFrame([]byte(str))); got != str {
		t.Fatalf("string data should pass through, got %s", got)
	}

	// Garbage passes through untouched.
	if got := string(normalizeClientFrame([]byte("nope"))); got != "nope" {
		t.Fatalf("unparseable payload should pass through, got %q", got)
	}
}

func TestInternalAPIKeyAuth(t *testing.T) {
	mk := func(cfg Config) *httptest.Server {
		s := &Server{
			cfg:     cfg,
			hub:     newHub(8),
			reg:     newRegistry(newFakeBackend(), nil, time.Minute),
			lifeCtx: context.Background(),
		}
		srv := httptest.NewServer(s.internalMux())
		t.Cleanup(srv.Close)
		return srv
	}
	get := func(url, key string) int {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if key != "" {
			req.Header.Set("x-internal-auth-key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// dev + no key → open (local development posture).
	dev := mk(Config{Env: "dev"})
	if code := get(dev.URL+"/internal/presence", ""); code != http.StatusOK {
		t.Fatalf("dev without key should be open, got %d", code)
	}

	// prod + no key → fail closed.
	prod := mk(Config{Env: "prod"})
	if code := get(prod.URL+"/internal/presence", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("prod without key must fail closed, got %d", code)
	}
	if code := get(prod.URL+"/internal/presence", "anything"); code != http.StatusServiceUnavailable {
		t.Fatalf("prod without key must reject even with a header, got %d", code)
	}

	// Keyed: correct key passes, wrong/mismatched-length keys are 401.
	keyed := mk(Config{Env: "prod", InternalAPIKey: "s3cret-key"})
	if code := get(keyed.URL+"/internal/presence", "s3cret-key"); code != http.StatusOK {
		t.Fatalf("correct key should pass, got %d", code)
	}
	if code := get(keyed.URL+"/internal/presence", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong key should be 401, got %d", code)
	}
	if code := get(keyed.URL+"/internal/presence", ""); code != http.StatusUnauthorized {
		t.Fatalf("missing key should be 401, got %d", code)
	}
}

func TestSlowConsumerDropped(t *testing.T) {
	s, pub, _ := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, wsURL(pub, testToken(t, "slow-user")), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()
	// Never read: let the outbound queue fill.
	var connID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := s.hub.localConns("slow-user"); len(ids) == 1 {
			connID = ids[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if connID == "" {
		t.Fatal("connection never registered")
	}
	// Fill the queue; the offer that overflows must drop the conn.
	for i := 0; i < s.cfg.SendBuffer+2; i++ {
		s.hub.offer(connID, []byte(fmt.Sprintf("m%d", i)))
	}
	if s.hub.has(connID) {
		t.Fatal("slow consumer was not dropped")
	}
}
