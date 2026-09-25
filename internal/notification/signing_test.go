package notification

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestSignedURLRoundTrip(t *testing.T) {
	signer := NewURLSigner("test-secret")
	base, _ := url.Parse("https://gateway.macro.com/notification")
	u := appendURLPath(base, "/user_notifications/preferences/email-digest-notification/disable")
	q := u.Query()
	q.Set("id", "macro|user@example.com")
	u.RawQuery = q.Encode()

	signed, err := signer.Sign(u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(signed.String(), "sig=") {
		t.Fatalf("no sig param: %s", signed)
	}
	if !strings.Contains(signed.String(), "/notification/user_notifications/") {
		t.Fatalf("path prefix lost: %s", signed)
	}

	// Verify against the URL the client would request (scheme://host + path
	// + query), mirroring Rust public_request_url.
	toVerify, _ := url.Parse("https://gateway.macro.com" + signed.RequestURI())
	if !signer.Verify(toVerify) {
		t.Fatal("round-trip verify failed")
	}

	// Tampered id must fail (id arrives form-encoded, so tamper the path).
	tampered, _ := url.Parse(strings.Replace(toVerify.String(), "email-digest-notification", "channel_mention", 1))
	if signer.Verify(tampered) {
		t.Fatal("tampered url verified")
	}

	// Missing sig must fail.
	unsigned, _ := url.Parse("https://gateway.macro.com/notification/user_notifications/preferences/x/disable?id=macro|u")
	if signer.Verify(unsigned) {
		t.Fatal("unsigned url verified")
	}
}

type disableRepo struct {
	Repository
	disabled map[string][]string
	err      error
}

func (r *disableRepo) DisableNotificationType(_ context.Context, userID, eventType string) error {
	if r.err != nil {
		return r.err
	}
	if r.disabled == nil {
		r.disabled = map[string][]string{}
	}
	r.disabled[userID] = append(r.disabled[userID], eventType)
	return nil
}

func TestPresignedDisableType(t *testing.T) {
	secret := "s3cret"
	signer := NewURLSigner(secret)
	repo := &disableRepo{}
	h := Handlers{
		svc:       &Service{repo: repo},
		signer:    signer,
		publicURL: "https://notif.example.com",
	}

	router := chi.NewRouter()
	router.Get("/user_notifications/preferences/{notification_event_type}/disable", h.presignedDisableType)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// Build a signed link as the digest flusher would (host rewritten to the
	// public URL the client hits).
	unsigned, _ := url.Parse("https://notif.example.com/user_notifications/preferences/channel_mention/disable")
	q := unsigned.Query()
	q.Set("id", "macro|u@x.com")
	unsigned.RawQuery = q.Encode()
	if _, err := signer.Sign(unsigned); err != nil {
		t.Fatal(err)
	}

	// The handler reconstructs {publicURL scheme}://{request Host}{uri}, so
	// sign a URL with the configured https scheme and the server's host —
	// matching production, where the configured URL's scheme is fixed but
	// the Host header is whatever the client connected to.
	real, _ := url.Parse(srv.URL)
	unsigned2, _ := url.Parse("https://" + real.Host + "/user_notifications/preferences/channel_mention/disable")
	unsigned2.RawQuery = q.Encode()
	signed2, err := signer.Sign(unsigned2)
	if err != nil {
		t.Fatal(err)
	}
	// httptest serves plain http; swap the scheme — the handler re-binds it
	// to the configured public scheme when verifying.
	requestURL := "http://" + real.Host + signed2.RequestURI()

	resp, err := srv.Client().Get(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("signed request rejected: %d", resp.StatusCode)
	}
	if got := repo.disabled["macro|u@x.com"]; len(got) != 1 || got[0] != "channel_mention" {
		t.Fatalf("type not disabled: %v", repo.disabled)
	}

	// Bad signature → 400.
	bad := srv.URL + "/user_notifications/preferences/channel_mention/disable?id=macro|u@x.com&sig=deadbeef"
	resp2, err := srv.Client().Get(bad)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("bad signature accepted: %d", resp2.StatusCode)
	}

	// Unblockable type with valid signature → 400.
	unsigned3, _ := url.Parse("https://" + real.Host + "/user_notifications/preferences/not_blockable/disable")
	unsigned3.RawQuery = q.Encode()
	signed3, _ := signer.Sign(unsigned3)
	resp3, err := srv.Client().Get("http://" + real.Host + signed3.RequestURI())
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 400 {
		t.Fatalf("unblockable type accepted: %d", resp3.StatusCode)
	}
}

func TestPushEventDeliveryFailureDrivesDigest(t *testing.T) {
	repo := &receiptRepo{
		userID:  "macro|u@x.com",
		notifID: uuid.New(),
		allFail: true,
		row:     ptr(testRow("macro|u@x.com")),
	}
	batcher := &fakeBatcher{}
	h := NewPushEventHandler(repo, batcher, 30*time.Second)

	err := h.Handle(context.Background(), PushEvent{
		Token:      "tok",
		DeviceType: string(DeviceIOSVoIP),
		EventType:  PushEventDeliveryFailure,
		MessageID:  "m1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != "iosvoip:tok" {
		t.Fatalf("voip-scoped delete expected, got %v", repo.deleted)
	}
	if len(batcher.rows) != 1 {
		t.Fatal("all-failed receipt should batch digest")
	}
}

func TestPushEventNotAllFailed(t *testing.T) {
	repo := &receiptRepo{userID: "u", notifID: uuid.New(), allFail: false}
	batcher := &fakeBatcher{}
	h := NewPushEventHandler(repo, batcher, 30*time.Second)
	err := h.Handle(context.Background(), PushEvent{
		Token:     "tok",
		EventType: PushEventDeliveryFailure,
		MessageID: "m1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(batcher.rows) != 0 {
		t.Fatal("partial failure must not batch")
	}
	// Untyped event → bare token delete.
	if len(repo.deleted) != 1 || repo.deleted[0] != "tok" {
		t.Fatalf("unexpected delete: %v", repo.deleted)
	}
}

type receiptRepo struct {
	Repository
	deleted []string
	userID  string
	notifID uuid.UUID
	allFail bool
	row     *Row
}

func (r *receiptRepo) DeleteDeviceByEndpoint(_ context.Context, e string) error {
	r.deleted = append(r.deleted, e)
	return nil
}

func (r *receiptRepo) MarkMessageFailed(_ context.Context, _ string) (string, uuid.UUID, error) {
	return r.userID, r.notifID, nil
}

func (r *receiptRepo) DidAllMessagesFail(_ context.Context, _ string, _ uuid.UUID) (bool, error) {
	return r.allFail, nil
}

func (r *receiptRepo) GetUserNotificationByID(_ context.Context, _ string, _ uuid.UUID) (*Row, error) {
	return r.row, nil
}

func ptr[T any](v T) *T { return &v }
