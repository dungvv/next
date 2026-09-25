package identity

import (
	"errors"
	"testing"
	"time"
)

func testValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator(Config{
		SessionJWTSecret:   "test-secret-that-is-long-enough",
		SessionJWTIssuer:   "macro",
		SessionJWTAudience: "aud",
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func TestRefreshTokenRoundTrip(t *testing.T) {
	i := testIssuer(t)
	v := testValidator(t)

	tok, id, err := i.IssueRefreshToken("macro|a@b.c", "a@b.c")
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty jti")
	}
	claims, err := v.ValidateRefreshToken(tok)
	if err != nil {
		t.Fatalf("ValidateRefreshToken: %v", err)
	}
	if claims.Subject != "macro|a@b.c" {
		t.Fatalf("unexpected subject %q", claims.Subject)
	}
	if claims.ID != id {
		t.Fatalf("jti mismatch: %q != %q", claims.ID, id)
	}
	if claims.ExpiresAt.Before(time.Now()) {
		t.Fatal("expected future expiry")
	}
}

func TestRefreshTokenIDsAreUnique(t *testing.T) {
	i := testIssuer(t)
	_, id1, _ := i.IssueRefreshToken("u", "e")
	_, id2, _ := i.IssueRefreshToken("u", "e")
	if id1 == id2 {
		t.Fatal("jtis must be unique")
	}
}

func TestAccessTokenIsNotARefreshToken(t *testing.T) {
	i := testIssuer(t)
	v := testValidator(t)

	access, err := i.IssueAccessToken(AccessClaims{
		MacroUserID: "macro|a@b.c", Email: "a@b.c",
	})
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if _, err := v.ValidateRefreshToken(access); err == nil {
		t.Fatal("access token must not validate as a refresh token")
	}
}

func TestRefreshTokenExpired(t *testing.T) {
	i := testIssuer(t)
	i.refreshTTL = -time.Minute // force expiry
	v := testValidator(t)

	tok, _, err := i.IssueRefreshToken("u", "e")
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	if _, err := v.ValidateRefreshToken(tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
	// ...but the allow-expired decode still yields claims for logout revoke.
	claims, err := v.DecodeRefreshTokenAllowExpired(tok)
	if err != nil {
		t.Fatalf("DecodeRefreshTokenAllowExpired: %v", err)
	}
	if claims.Subject != "u" {
		t.Fatalf("unexpected subject %q", claims.Subject)
	}
}

func TestRefreshTokenWrongKey(t *testing.T) {
	i := testIssuer(t)
	v2, err := NewValidator(Config{
		SessionJWTSecret:   "a-different-secret-key-material",
		SessionJWTIssuer:   "macro",
		SessionJWTAudience: "aud",
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	tok, _, err := i.IssueRefreshToken("u", "e")
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	if _, err := v2.ValidateRefreshToken(tok); err == nil {
		t.Fatal("expected validation failure under a different key")
	}
}
