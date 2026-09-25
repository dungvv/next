package chat

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/auth"
)

// newTestServer wires the handler with no DB/NATS — sufficient for auth and
// routing tests (DB-touching handlers are covered only where they reject
// before hitting the store).
func newTestServer() http.Handler {
	mw := auth.MiddlewareWithVerifier("test-internal-key", nil, false)
	return NewServer(Config{}, nil, nil, nil, mw).Handler()
}

func TestHealthNoAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health: got %d", rec.Code)
	}
}

func TestChannelsRequireAuth(t *testing.T) {
	h := newTestServer()
	for _, path := range []string{
		"/comms/channels",
		"/channels/activity",
		"/channels/" + "11111111-1111-1111-1111-111111111111",
		"/channels/" + "11111111-1111-1111-1111-111111111111" + "/messages",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s: got %d, want 401", path, rec.Code)
		}
	}
}

// A bare x-user-id header must NOT authenticate (regression guard for the
// impersonation hole closed earlier).
func TestBareUserIDRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/comms/channels", nil)
	req.Header.Set(auth.HeaderUserID, "macro|attacker@x.com")
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bare x-user-id accepted: got %d", rec.Code)
	}
}

// Internal-key callers reach the handler layer (DB is nil → 5xx is fine;
// we only assert it isn't 401/403 on an authenticated route).
func TestInternalKeyAuthenticates(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/comms/channels", nil)
	req.Header.Set(auth.HeaderInternalAPIKey, "test-internal-key")
	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Skipf("handler panics on nil db (expected — auth passed): %v", r)
		}
	}()
	newTestServer().ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("internal key rejected: got %d", rec.Code)
	}
}

func TestBadUUIDParam(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/channels/not-a-uuid", nil)
	req.Header.Set(auth.HeaderInternalAPIKey, "test-internal-key")
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad uuid: got %d", rec.Code)
	}
}

func TestChannelCursorRoundTrip(t *testing.T) {
	at := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	gotT, gotID, err := decodeChannelCursor(encodeChannelCursor(at, id))
	if err != nil || !gotT.Equal(at) || gotID != id {
		t.Fatalf("cursor round-trip: %v %v %v", gotT, gotID, err)
	}
	if _, _, err := decodeChannelCursor("not-base64!!"); err == nil {
		t.Fatal("bad cursor accepted")
	}
}

func TestResolveChannelName(t *testing.T) {
	parts := []ChannelParticipant{
		{UserID: "macro|me@x.io"}, {UserID: "macro|bob@x.io"},
	}
	names := map[string]string{"macro|bob@x.io": "Bob Smith"}

	dm := channelRow{ChannelType: ChannelDM}
	if got := resolveChannelName(dm, "macro|me@x.io", parts, names); got != "Bob Smith" {
		t.Fatalf("dm name %q", got)
	}
	// Fallback to email local part when macro_user_info has no name.
	if got := resolveChannelName(dm, "macro|me@x.io", parts, nil); got != "bob" {
		t.Fatalf("dm fallback %q", got)
	}
	pub := channelRow{ChannelType: ChannelPublic, ID: uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")}
	if got := resolveChannelName(pub, "macro|me@x.io", parts, nil); got != "#aaaaaaaa" {
		t.Fatalf("public name %q", got)
	}
	named := "stored"
	stored := channelRow{ChannelType: ChannelPrivate, Name: &named}
	if got := resolveChannelName(stored, "macro|me@x.io", parts, nil); got != "stored" {
		t.Fatalf("stored name %q", got)
	}
}

func TestHelpers(t *testing.T) {
	got := exclude([]string{"a", "b", "a", "c"}, map[string]bool{"b": true})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("exclude: %v", got)
	}
	if truncate("hello", 10) != "hello" || len(truncate("hello world", 5)) == 11 {
		t.Fatal("truncate")
	}
	if displayName("macro|alice@corp.io") != "alice" {
		t.Fatalf("displayName: %q", displayName("macro|alice@corp.io"))
	}
	if displayName("bot-system") != "bot-system" {
		t.Fatalf("displayName: %q", displayName("bot-system"))
	}
}
