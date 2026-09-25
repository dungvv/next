package gmailx

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func testKeyB64(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestCipherRoundTrip(t *testing.T) {
	c, err := newTokenCipher(testKeyB64(t))
	if err != nil {
		t.Fatalf("newTokenCipher: %v", err)
	}
	link := uuid.New()
	ct, err := c.Encrypt(link, "refresh-token-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ct == "refresh-token-secret" {
		t.Fatal("ciphertext must differ from plaintext")
	}
	pt, err := c.Decrypt(link, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "refresh-token-secret" {
		t.Fatalf("round-trip mismatch: %q", pt)
	}
}

func TestCipherRejectsTamper(t *testing.T) {
	c, _ := newTokenCipher(testKeyB64(t))
	link := uuid.New()
	ct, _ := c.Encrypt(link, "secret")
	raw := []byte(ct)
	raw[len(raw)-5] ^= 0x01
	if _, err := c.Decrypt(link, string(raw)); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestCipherRejectsWrongLink(t *testing.T) {
	c, _ := newTokenCipher(testKeyB64(t))
	ct, _ := c.Encrypt(uuid.New(), "secret")
	if _, err := c.Decrypt(uuid.New(), ct); err == nil {
		t.Fatal("ciphertext must not decrypt under a different link (AAD)")
	}
}

func TestCipherRejectsWrongKey(t *testing.T) {
	a, _ := newTokenCipher(testKeyB64(t))
	b, _ := newTokenCipher(testKeyB64(t))
	link := uuid.New()
	ct, _ := a.Encrypt(link, "secret")
	if _, err := b.Decrypt(link, ct); err == nil {
		t.Fatal("ciphertext must not decrypt under a different key")
	}
}

func TestCipherRejectsPlaintext(t *testing.T) {
	c, _ := newTokenCipher(testKeyB64(t))
	if _, err := c.Decrypt(uuid.New(), "plaintext-token"); !errors.Is(err, ErrNotCiphertext) {
		t.Fatalf("expected ErrNotCiphertext, got %v", err)
	}
}

func TestNewTokenCipherKeyValidation(t *testing.T) {
	if _, err := newTokenCipher("!!!not-base64!!!"); err == nil {
		t.Fatal("expected base64 decode failure")
	}
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if _, err := newTokenCipher(short); err == nil {
		t.Fatal("expected 32-byte length check")
	}
	if _, err := newTokenCipher(testKeyB64(t)); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
}

func TestCipherUniqueNonces(t *testing.T) {
	c, _ := newTokenCipher(testKeyB64(t))
	link := uuid.New()
	a, _ := c.Encrypt(link, "same")
	b, _ := c.Encrypt(link, "same")
	if a == b {
		t.Fatal("identical plaintexts must produce distinct ciphertexts")
	}
}
