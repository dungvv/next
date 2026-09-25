package gmailx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// TokenStore persists Gmail OAuth2 tokens per email link. The table is
// managed locally (no migration dependency) because the Rust side stored
// tokens in FusionAuth grants, which don't exist in the self-host world —
// tokens are seeded here by the OAuth connect flow (or manually) instead.
//
// Tokens are encrypted at rest when a cipher is configured
// (TOKEN_ENCRYPTION_KEY → AES-256-GCM, see cipher.go). With no key the store
// writes plaintext and warns once — self-host installs without the key keep
// working, but production deployments should always set it.
type TokenStore struct {
	pool     *pgxpool.Pool
	oauthCfg *oauth2.Config
	cipher   *tokenCipher
}

// NewTokenStore builds the store. clientID/secret are the Google OAuth app
// credentials used to refresh access tokens; encryptionKeyB64 is a
// base64-encoded 32-byte AES key (TOKEN_ENCRYPTION_KEY) — empty disables
// at-rest encryption.
func NewTokenStore(pool *pgxpool.Pool, clientID, clientSecret, encryptionKeyB64 string) (*TokenStore, error) {
	s := &TokenStore{
		pool: pool,
		oauthCfg: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     google.Endpoint,
			Scopes: []string{
				"https://www.googleapis.com/auth/gmail.modify",
				"https://www.googleapis.com/auth/gmail.settings.basic",
			},
		},
	}
	if encryptionKeyB64 != "" {
		c, err := newTokenCipher(encryptionKeyB64)
		if err != nil {
			return nil, err
		}
		s.cipher = c
	} else {
		slog.Warn("gmailx: TOKEN_ENCRYPTION_KEY unset — gmail tokens stored plaintext")
	}
	return s, nil
}

// encrypt seals a token for the link; plaintext passthrough when no cipher.
func (s *TokenStore) encrypt(linkID uuid.UUID, token string) (string, error) {
	if s.cipher == nil || token == "" {
		return token, nil
	}
	return s.cipher.Encrypt(linkID, token)
}

// decrypt opens a stored token for the link. With a cipher configured a
// non-ciphertext or undecryptable value is a hard error — never a plaintext
// fallback.
func (s *TokenStore) decrypt(linkID uuid.UUID, stored string) (string, error) {
	if s.cipher == nil {
		return stored, nil
	}
	return s.cipher.Decrypt(linkID, stored)
}

// VerifyMailbox exchanges the refresh token for an access token and returns
// the Gmail profile's email address — the ownership proof the link flow
// checks before accepting a caller-supplied mailbox.
func (s *TokenStore) VerifyMailbox(ctx context.Context, refreshToken string) (string, error) {
	src := s.oauthCfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	tok, err := src.Token()
	if err != nil {
		return "", fmt.Errorf("gmailx: token exchange: %w", err)
	}
	gc := New(staticTokenSource{token: tok})
	p, err := gc.GetProfile(ctx)
	if err != nil {
		return "", fmt.Errorf("gmailx: get profile: %w", err)
	}
	if p.EmailAddress == "" {
		return "", errors.New("gmailx: profile has no email address")
	}
	return p.EmailAddress, nil
}

// staticTokenSource serves a fixed token — used by VerifyMailbox before any
// link/token row exists.
type staticTokenSource struct {
	token *oauth2.Token
}

func (s staticTokenSource) Token(context.Context) (*oauth2.Token, error) {
	return s.token, nil
}

// EnsureSchema creates email_gmail_tokens when absent.
func (s *TokenStore) EnsureSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS email_gmail_tokens (
			link_id       uuid PRIMARY KEY,
			refresh_token text NOT NULL,
			access_token  text,
			expiry        timestamptz,
			scopes        text[] NOT NULL DEFAULT '{}',
			updated_at    timestamptz NOT NULL DEFAULT now()
		)`)
	return err
}

// Put stores (upserts) a refresh token for a link — called by the OAuth
// connect flow once tokens are provisioned. The token is encrypted at rest
// when the store has a cipher.
func (s *TokenStore) Put(ctx context.Context, linkID uuid.UUID, refreshToken string, scopes []string) error {
	stored, err := s.encrypt(linkID, refreshToken)
	if err != nil {
		return fmt.Errorf("gmailx: encrypt refresh token: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO email_gmail_tokens (link_id, refresh_token, scopes)
		VALUES ($1, $2, $3)
		ON CONFLICT (link_id) DO UPDATE SET
			refresh_token = EXCLUDED.refresh_token,
			scopes = EXCLUDED.scopes,
			access_token = NULL,
			expiry = NULL,
			updated_at = now()`, linkID, stored, scopes)
	return err
}

// Delete removes a link's stored token.
func (s *TokenStore) Delete(ctx context.Context, linkID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM email_gmail_tokens WHERE link_id = $1`, linkID)
	return err
}

// Has reports whether a refresh token exists for the link.
func (s *TokenStore) Has(ctx context.Context, linkID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM email_gmail_tokens WHERE link_id = $1)`, linkID).Scan(&exists)
	return exists, err
}

// ErrNoToken is returned when no refresh token is stored for the link.
var ErrNoToken = fmt.Errorf("gmailx: no token for link")

// Source returns a TokenSource bound to one link that refreshes via the
// stored refresh token and persists renewed access tokens.
func (s *TokenStore) Source(linkID uuid.UUID) TokenSource {
	return &dbTokenSource{store: s, linkID: linkID}
}

type dbTokenSource struct {
	store  *TokenStore
	linkID uuid.UUID
	cached *oauth2.Token
}

func (t *dbTokenSource) Token(ctx context.Context) (*oauth2.Token, error) {
	if t.cached != nil && t.cached.Valid() {
		return t.cached, nil
	}
	var refreshEnc, accessEnc *string
	var expiry *time.Time
	err := t.store.pool.QueryRow(ctx,
		`SELECT refresh_token, access_token, expiry FROM email_gmail_tokens WHERE link_id = $1`,
		t.linkID).Scan(&refreshEnc, &accessEnc, &expiry)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", ErrNoToken, t.linkID)
	}
	if refreshEnc == nil {
		return nil, fmt.Errorf("%w (%s)", ErrNoToken, t.linkID)
	}
	refresh, err := t.store.decrypt(t.linkID, *refreshEnc)
	if err != nil {
		return nil, fmt.Errorf("gmailx: decrypt refresh token for link %s: %w", t.linkID, err)
	}
	// Cached DB access token still valid → use it.
	if accessEnc != nil && expiry != nil && time.Until(*expiry) > time.Minute {
		access, err := t.store.decrypt(t.linkID, *accessEnc)
		if err != nil {
			return nil, fmt.Errorf("gmailx: decrypt access token for link %s: %w", t.linkID, err)
		}
		tok := &oauth2.Token{AccessToken: access, Expiry: *expiry, RefreshToken: refresh}
		t.cached = tok
		return tok, nil
	}
	// Refresh through Google's token endpoint.
	src := t.store.oauthCfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh})
	tok, err := src.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh gmail token: %w", err)
	}
	// Persist the new access token (and rotated refresh token, if returned).
	newRefresh := refresh
	if tok.RefreshToken != "" {
		newRefresh = tok.RefreshToken
	}
	storedAccess, err := t.store.encrypt(t.linkID, tok.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("gmailx: encrypt access token: %w", err)
	}
	storedRefresh, err := t.store.encrypt(t.linkID, newRefresh)
	if err != nil {
		return nil, fmt.Errorf("gmailx: encrypt refresh token: %w", err)
	}
	if _, err := t.store.pool.Exec(ctx, `
		UPDATE email_gmail_tokens SET access_token = $2, expiry = $3,
			refresh_token = $4, updated_at = now() WHERE link_id = $1`,
		t.linkID, storedAccess, tok.Expiry, storedRefresh); err != nil {
		// The fresh token is still returned to the caller, but the persist
		// failure is loud — a silent drop would wedge the next refresh.
		slog.Error("gmailx: persist refreshed token failed", "link_id", t.linkID, "err", err)
	}
	t.cached = tok
	return tok, nil
}
