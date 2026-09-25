package authentication

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Port of services/authentication_service/src/microsoft_token_cipher.rs —
// AES-256-GCM envelope encryption for Microsoft refresh tokens.
//
// The Rust version wrapped per-token data keys with AWS KMS. For self-hosting
// the KMS provider is replaced by a local master key
// (MICROSOFT_TOKEN_MASTER_KEY, base64-encoded 32 bytes) — same envelope
// format, same AAD, so ciphertexts are interchangeable once a real KMS
// provider is wired through DataKeyProvider.
//
// TODO(port): cursor API keys use the same envelope pattern with purpose
// "cursor-api-key" (CURSOR_API_KEY_MASTER_KEY) — share this cipher when the
// cursor-api-key routes are ported.

const (
	msEncryptionVersion    = int16(1)
	aes256KeyLength        = 32
	aesGCMNonceLength      = 12
	aesGCMTagLength        = 16
	msEncryptionPurpose    = "microsoft-refresh-token"
	masterKeyWrapKeyID     = "local-master-key"
	msCtxPurpose           = "macro:purpose"
	msCtxEncryptionVersion = "macro:encryption-version"
	msCtxFusionAuthUserID  = "macro:fusionauth-user-id"
	msCtxMailbox           = "macro:microsoft-mailbox"
)

// EncryptedMicrosoftToken mirrors EncryptedMicrosoftToken — the persisted
// envelope.
type EncryptedMicrosoftToken struct {
	RefreshTokenCiphertext []byte
	EncryptedDataKey       []byte
	Nonce                  []byte
	EncryptionVersion      int16
	// KeyID was the KMS key id; "local-master-key" for the local provider.
	KeyID string
}

// MicrosoftTokenCipherError kinds (mirrors the Rust enum).
var (
	ErrCipherMalformedIdentity  = errors.New("microsoft token identity is malformed")
	ErrCipherMalformedEnvelope  = errors.New("microsoft token envelope is malformed")
	ErrCipherMalformedPlaintext = errors.New("microsoft token plaintext is malformed")
	ErrCipherUnsupportedVersion = errors.New("microsoft token envelope uses unsupported encryption version")
	ErrCipherInvalidDataKey     = errors.New("microsoft token data key is invalid")
	ErrCipherEncryptionFailed   = errors.New("microsoft token encryption failed")
	ErrCipherDecryptionFailed   = errors.New("microsoft token decryption failed")
	ErrCipherGenerateDataKey    = errors.New("data-key generate failed")
	ErrCipherDecryptDataKey     = errors.New("data-key decrypt failed")
)

// generatedDataKey is a fresh plaintext+wrapped data key pair.
type generatedDataKey struct {
	plaintext []byte
	encrypted []byte
	keyID     string
}

// DataKeyProvider is the envelope key-wrap port — KMS in Rust, local master
// key here.
type DataKeyProvider interface {
	GenerateDataKey(ctx context.Context, encryptionContext map[string]string) (*generatedDataKey, error)
	DecryptDataKey(ctx context.Context, keyID string, encryptedDataKey []byte, encryptionContext map[string]string) ([]byte, error)
}

// MicrosoftTokenCipher encrypts/decrypts Microsoft refresh-token envelopes.
type MicrosoftTokenCipher struct {
	provider DataKeyProvider
}

// NewMicrosoftTokenCipher builds the cipher over a data-key provider.
func NewMicrosoftTokenCipher(p DataKeyProvider) *MicrosoftTokenCipher {
	return &MicrosoftTokenCipher{provider: p}
}

// encryptionIdentity is the AAD + key-wrap context identity.
type encryptionIdentity struct {
	providerUserID string
	emailAddress   string
}

func newEncryptionIdentity(providerUserID, emailAddress string) (*encryptionIdentity, error) {
	id := strings.ToLower(strings.TrimSpace(providerUserID))
	email := strings.ToLower(strings.TrimSpace(emailAddress))
	if id == "" || strings.ContainsRune(id, 0) || !isValidEmail(email) {
		return nil, ErrCipherMalformedIdentity
	}
	return &encryptionIdentity{providerUserID: id, emailAddress: email}, nil
}

// aad mirrors EncryptionIdentity::aad — purpose bytes, then length-prefixed
// version (i16 BE), user id, and mailbox.
func (e *encryptionIdentity) aad() []byte {
	var out []byte
	out = append(out, msEncryptionPurpose...)
	var v [2]byte
	binary.BigEndian.PutUint16(v[:], uint16(msEncryptionVersion))
	out = appendLengthPrefixed(out, v[:])
	out = appendLengthPrefixed(out, []byte(e.providerUserID))
	out = appendLengthPrefixed(out, []byte(e.emailAddress))
	return out
}

// encryptionContext mirrors kms_encryption_context — bound into the wrapped
// data key as AAD by the local provider (the KMS provider sent it as the KMS
// encryption context).
func (e *encryptionIdentity) encryptionContext() map[string]string {
	return map[string]string{
		msCtxPurpose:           msEncryptionPurpose,
		msCtxEncryptionVersion: fmt.Sprintf("%d", msEncryptionVersion),
		msCtxFusionAuthUserID:  e.providerUserID,
		msCtxMailbox:           e.emailAddress,
	}
}

func appendLengthPrefixed(out, value []byte) []byte {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(value)))
	out = append(out, n[:]...)
	return append(out, value...)
}

// Encrypt mirrors MicrosoftTokenCipher::encrypt.
func (c *MicrosoftTokenCipher) Encrypt(ctx context.Context, providerUserID, emailAddress, refreshToken string) (*EncryptedMicrosoftToken, error) {
	id, err := newEncryptionIdentity(providerUserID, emailAddress)
	if err != nil {
		return nil, err
	}
	dk, err := c.provider.GenerateDataKey(ctx, id.encryptionContext())
	if err != nil {
		return nil, err
	}
	if len(dk.plaintext) != aes256KeyLength {
		return nil, ErrCipherInvalidDataKey
	}
	defer zeroize(dk.plaintext)

	block, err := aes.NewCipher(dk.plaintext)
	if err != nil {
		return nil, ErrCipherInvalidDataKey
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrCipherEncryptionFailed
	}
	nonce := make([]byte, aesGCMNonceLength)
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrCipherEncryptionFailed
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(refreshToken), id.aad())

	return &EncryptedMicrosoftToken{
		RefreshTokenCiphertext: ciphertext,
		EncryptedDataKey:       dk.encrypted,
		Nonce:                  nonce,
		EncryptionVersion:      msEncryptionVersion,
		KeyID:                  dk.keyID,
	}, nil
}

// Decrypt mirrors MicrosoftTokenCipher::decrypt.
func (c *MicrosoftTokenCipher) Decrypt(ctx context.Context, providerUserID, emailAddress string, envelope *EncryptedMicrosoftToken) (string, error) {
	if envelope.EncryptionVersion != msEncryptionVersion {
		return "", fmt.Errorf("%w %d", ErrCipherUnsupportedVersion, envelope.EncryptionVersion)
	}
	if len(envelope.Nonce) != aesGCMNonceLength ||
		len(envelope.RefreshTokenCiphertext) < aesGCMTagLength ||
		len(envelope.EncryptedDataKey) == 0 ||
		strings.TrimSpace(envelope.KeyID) == "" {
		return "", ErrCipherMalformedEnvelope
	}
	id, err := newEncryptionIdentity(providerUserID, emailAddress)
	if err != nil {
		return "", err
	}
	plaintextKey, err := c.provider.DecryptDataKey(ctx, envelope.KeyID, envelope.EncryptedDataKey, id.encryptionContext())
	if err != nil {
		return "", err
	}
	defer zeroize(plaintextKey)
	if len(plaintextKey) != aes256KeyLength {
		return "", ErrCipherInvalidDataKey
	}
	block, err := aes.NewCipher(plaintextKey)
	if err != nil {
		return "", ErrCipherInvalidDataKey
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", ErrCipherDecryptionFailed
	}
	plaintext, err := gcm.Open(nil, envelope.Nonce, envelope.RefreshTokenCiphertext, id.aad())
	if err != nil {
		return "", ErrCipherDecryptionFailed
	}
	return string(plaintext), nil
}

func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// --- local master-key data-key provider --------------------------------------

// LocalDataKeyProvider wraps generated data keys with a static 32-byte master
// key — the self-host replacement for KMS. The encryption context is bound as
// AAD on the wrap, matching KMS encryption-context semantics.
type LocalDataKeyProvider struct {
	gcm cipher.AEAD
}

// NewLocalDataKeyProvider decodes a base64 32-byte master key.
func NewLocalDataKeyProvider(masterKeyB64 string) (*LocalDataKeyProvider, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(masterKeyB64))
	if err != nil {
		return nil, fmt.Errorf("identity cipher: decode MICROSOFT_TOKEN_MASTER_KEY: %w", err)
	}
	if len(key) != aes256KeyLength {
		return nil, fmt.Errorf("identity cipher: MICROSOFT_TOKEN_MASTER_KEY must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &LocalDataKeyProvider{gcm: gcm}, nil
}

// contextAAD renders the encryption context as deterministic AAD: sorted
// "k=v" pairs, each length-prefixed.
func contextAAD(ctx map[string]string) []byte {
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []byte
	for _, k := range keys {
		out = appendLengthPrefixed(out, []byte(k+"="+ctx[k]))
	}
	return out
}

func (p *LocalDataKeyProvider) GenerateDataKey(_ context.Context, encryptionContext map[string]string) (*generatedDataKey, error) {
	plaintext := make([]byte, aes256KeyLength)
	if _, err := rand.Read(plaintext); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCipherGenerateDataKey, err)
	}
	nonce := make([]byte, aesGCMNonceLength)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCipherGenerateDataKey, err)
	}
	wrapped := p.gcm.Seal(nil, nonce, plaintext, contextAAD(encryptionContext))
	// Wrapped blob = nonce || ciphertext (unambiguous since both are
	// fixed-length on open).
	encrypted := append(nonce, wrapped...)
	return &generatedDataKey{plaintext: plaintext, encrypted: encrypted, keyID: masterKeyWrapKeyID}, nil
}

func (p *LocalDataKeyProvider) DecryptDataKey(_ context.Context, keyID string, encryptedDataKey []byte, encryptionContext map[string]string) ([]byte, error) {
	if keyID != masterKeyWrapKeyID {
		return nil, fmt.Errorf("%w: unknown key id %q", ErrCipherDecryptDataKey, keyID)
	}
	if len(encryptedDataKey) < aesGCMNonceLength+aesGCMTagLength {
		return nil, fmt.Errorf("%w: malformed wrapped key", ErrCipherDecryptDataKey)
	}
	nonce, ct := encryptedDataKey[:aesGCMNonceLength], encryptedDataKey[aesGCMNonceLength:]
	plaintext, err := p.gcm.Open(nil, nonce, ct, contextAAD(encryptionContext))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCipherDecryptDataKey, err)
	}
	return plaintext, nil
}
