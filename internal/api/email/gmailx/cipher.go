package gmailx

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Token at-rest encryption.
//
// The Rust stack kept Gmail grants inside FusionAuth (encrypted by the
// provider) and wrapped Microsoft refresh tokens in a KMS envelope
// (services/authentication_service/src/microsoft_token_cipher.rs). Neither
// applies to self-host rows, so this is a NEW format — not wire-compatible
// with anything Rust wrote:
//
//	v1:<base64(nonce || ciphertext)>
//
// AES-256-GCM keyed by TOKEN_ENCRYPTION_KEY (base64, 32 bytes), with the
// link id bound as AAD so a ciphertext row can't be transplanted onto a
// different link. Modeled on microsoft_token_cipher's local master-key
// provider, minus the per-token data-key wrap (the column holds one secret
// per row, so a second envelope layer buys nothing operationally).

const (
	tokenCipherPrefix   = "v1:"
	tokenCipherKeyBytes = 32
	tokenNonceBytes     = 12
)

// ErrNotCiphertext is returned when a stored value lacks the v1: prefix —
// either a legacy plaintext row or tampering; either way it must not be used.
var ErrNotCiphertext = errors.New("gmailx: stored token is not v1 ciphertext")

type tokenCipher struct {
	gcm cipher.AEAD
}

// newTokenCipher builds the cipher from a base64-encoded 32-byte key.
func newTokenCipher(keyB64 string) (*tokenCipher, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil {
		return nil, fmt.Errorf("gmailx: decode TOKEN_ENCRYPTION_KEY: %w", err)
	}
	if len(key) != tokenCipherKeyBytes {
		return nil, fmt.Errorf("gmailx: TOKEN_ENCRYPTION_KEY must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &tokenCipher{gcm: gcm}, nil
}

func (c *tokenCipher) aad(linkID uuid.UUID) []byte {
	return []byte("gmail-token:" + linkID.String())
}

// Encrypt seals plaintext for the link; the output is a `v1:`-prefixed
// base64 string safe for the text column.
func (c *tokenCipher) Encrypt(linkID uuid.UUID, plaintext string) (string, error) {
	nonce := make([]byte, tokenNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("gmailx: token nonce: %w", err)
	}
	ct := c.gcm.Seal(nonce, nonce, []byte(plaintext), c.aad(linkID))
	return tokenCipherPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt opens a `v1:` ciphertext for the link. Any failure is terminal —
// there is no plaintext fallback.
func (c *tokenCipher) Decrypt(linkID uuid.UUID, stored string) (string, error) {
	raw, ok := strings.CutPrefix(stored, tokenCipherPrefix)
	if !ok {
		return "", ErrNotCiphertext
	}
	ct, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(ct) < tokenNonceBytes+c.gcm.Overhead() {
		return "", fmt.Errorf("gmailx: malformed token ciphertext")
	}
	pt, err := c.gcm.Open(nil, ct[:tokenNonceBytes], ct[tokenNonceBytes:], c.aad(linkID))
	if err != nil {
		return "", fmt.Errorf("gmailx: token decrypt failed: %w", err)
	}
	return string(pt), nil
}
