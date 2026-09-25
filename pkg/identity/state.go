package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Signed OAuth state.
//
// The login flow round-trips an opaque `state` param through the identity
// provider. Because the provider (and the browser) can both observe and
// replay it, the state is wrapped in an HMAC-signed envelope carrying a
// nonce + expiry; the nonce is also set as a cookie so the callback can bind
// the authorization response to the browser that started the flow (the
// standard OIDC login-CSRF defense).
//
// Wire format: base64url(json payload) "." base64url(hmac-sha256(payload)).

// OAuthStateTTL is how long a signed state stays valid — long enough for the
// user to complete the provider login, short enough to limit replay windows.
const OAuthStateTTL = 10 * time.Minute

type oauthStatePayload struct {
	// Nonce binds the state to the browser cookie set at /login/sso.
	Nonce string `json:"nonce"`
	// ExpiresAt is a unix timestamp; expired states are rejected.
	ExpiresAt int64 `json:"exp"`
	// Data is the caller's inner state blob (base64-encoded JSON).
	Data []byte `json:"data,omitempty"`
}

// ErrOAuthState covers every state-verification failure; the handler maps it
// to a generic 400 so an attacker can't distinguish signature/expiry/nonce
// failures.
var ErrOAuthState = errors.New("identity: invalid oauth state")

// SignOAuthState wraps data in a signed envelope bound to nonce, expiring
// after ttl.
func (i *Issuer) SignOAuthState(data []byte, nonce string, ttl time.Duration) (string, error) {
	payload, err := json.Marshal(oauthStatePayload{
		Nonce:     nonce,
		ExpiresAt: time.Now().Add(ttl).Unix(),
		Data:      data,
	})
	if err != nil {
		return "", fmt.Errorf("identity: marshal oauth state: %w", err)
	}
	seg := base64.RawURLEncoding.EncodeToString(payload)
	return seg + "." + i.stateSignature(seg), nil
}

// VerifyOAuthState checks the envelope signature, expiry, and nonce binding
// and returns the inner data. expectNonce is the value from the browser
// cookie; a mismatch (or any other failure) yields ErrOAuthState.
func (i *Issuer) VerifyOAuthState(raw, expectNonce string) ([]byte, error) {
	seg, sig, ok := splitLastByte(raw, '.')
	if !ok || seg == "" || sig == "" {
		return nil, ErrOAuthState
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(i.stateSignature(seg))) != 1 {
		return nil, ErrOAuthState
	}
	payload, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return nil, ErrOAuthState
	}
	var st oauthStatePayload
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, ErrOAuthState
	}
	if st.ExpiresAt <= time.Now().Unix() {
		return nil, ErrOAuthState
	}
	if st.Nonce == "" || expectNonce == "" ||
		subtle.ConstantTimeCompare([]byte(st.Nonce), []byte(expectNonce)) != 1 {
		return nil, ErrOAuthState
	}
	return st.Data, nil
}

func (i *Issuer) stateSignature(seg string) string {
	mac := hmac.New(sha256.New, i.stateKey)
	mac.Write([]byte(seg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func splitLastByte(s string, b byte) (string, string, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
