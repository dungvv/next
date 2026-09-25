package identity

import (
	"strings"
	"testing"
	"time"
)

func testIssuer(t *testing.T) *Issuer {
	t.Helper()
	i, err := NewIssuer(Config{
		SessionJWTSecret:   "test-secret-that-is-long-enough",
		SessionJWTIssuer:   "macro",
		SessionJWTAudience: "aud",
		AccessTokenTTL:     time.Hour,
		RefreshTokenTTL:    24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return i
}

func TestOAuthStateRoundTrip(t *testing.T) {
	i := testIssuer(t)
	state, err := i.SignOAuthState([]byte("inner-state"), "nonce-1", OAuthStateTTL)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	data, err := i.VerifyOAuthState(state, "nonce-1")
	if err != nil {
		t.Fatalf("VerifyOAuthState: %v", err)
	}
	if string(data) != "inner-state" {
		t.Fatalf("unexpected inner data %q", data)
	}
}

func TestOAuthStateRejectsTampering(t *testing.T) {
	i := testIssuer(t)
	state, err := i.SignOAuthState([]byte("x"), "nonce-1", OAuthStateTTL)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	// Flip a character inside the payload segment.
	tampered := state[:4] + "X" + state[5:]
	if _, err := i.VerifyOAuthState(tampered, "nonce-1"); err == nil {
		t.Fatal("expected tampered state to be rejected")
	}
}

func TestOAuthStateRejectsWrongNonce(t *testing.T) {
	i := testIssuer(t)
	state, err := i.SignOAuthState([]byte("x"), "nonce-1", OAuthStateTTL)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	if _, err := i.VerifyOAuthState(state, "nonce-2"); err == nil {
		t.Fatal("expected nonce mismatch to be rejected")
	}
	if _, err := i.VerifyOAuthState(state, ""); err == nil {
		t.Fatal("expected empty nonce to be rejected")
	}
}

func TestOAuthStateRejectsExpired(t *testing.T) {
	i := testIssuer(t)
	state, err := i.SignOAuthState([]byte("x"), "nonce-1", -time.Minute)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	if _, err := i.VerifyOAuthState(state, "nonce-1"); err == nil {
		t.Fatal("expected expired state to be rejected")
	}
}

func TestOAuthStateRejectsMalformed(t *testing.T) {
	i := testIssuer(t)
	for _, s := range []string{"", "nosplitchar", ".sig", "payload.", "a.b.c"} {
		if _, err := i.VerifyOAuthState(s, "nonce"); err == nil {
			t.Fatalf("expected %q to be rejected", s)
		}
	}
}

func TestOAuthStateRejectedByDifferentIssuer(t *testing.T) {
	a := testIssuer(t)
	b, err := NewIssuer(Config{
		SessionJWTSecret:   "a-different-secret-key-material",
		SessionJWTIssuer:   "macro",
		SessionJWTAudience: "aud",
		AccessTokenTTL:     time.Hour,
		RefreshTokenTTL:    24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	state, err := a.SignOAuthState([]byte("x"), "nonce-1", OAuthStateTTL)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	if _, err := b.VerifyOAuthState(state, "nonce-1"); err == nil {
		t.Fatal("expected state signed by another key to be rejected")
	}
}

func TestSplitLastByte(t *testing.T) {
	seg, sig, ok := splitLastByte("a.b", '.')
	if !ok || seg != "a" || sig != "b" {
		t.Fatalf("got %q %q %v", seg, sig, ok)
	}
	if strings.Contains("a.b", "..") {
		t.Fatal("sanity")
	}
	if _, _, ok := splitLastByte("abc", '.'); ok {
		t.Fatal("expected no split")
	}
}
