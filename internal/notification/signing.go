package notification

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// This file ports crates/notification/src/domain/models/signing.rs:
// presigned URLs carry a hex HMAC-SHA256 signature in the `sig` query
// parameter, computed over the canonicalized URL string with `sig` absent.

const sigParam = "sig"

// URLSigner signs and verifies presigned URLs (URL_SIGNING_HMAC).
type URLSigner struct {
	key []byte
}

// NewURLSigner builds a signer from the raw secret; nil when empty.
func NewURLSigner(secret string) *URLSigner {
	if secret == "" {
		return nil
	}
	return &URLSigner{key: []byte(secret)}
}

// queryPairs returns the ordered decoded query pairs of u. Rust's
// url::Url::query_pairs performs form-urlencoded decoding and preserves
// ordering; url.Values does not, so we parse the raw query ourselves.
func queryPairs(u *url.URL) [][2]string {
	raw := u.RawQuery
	if raw == "" {
		return nil
	}
	var pairs [][2]string
	for _, field := range strings.Split(raw, "&") {
		if field == "" {
			continue
		}
		k, v, _ := strings.Cut(field, "=")
		dk, _ := url.QueryUnescape(k)
		dv, _ := url.QueryUnescape(v)
		pairs = append(pairs, [2]string{dk, dv})
	}
	return pairs
}

// encodePairs re-encodes pairs in order, mirroring the ordered
// form-urlencoded serialization url::Url::query_pairs_mut produces.
func encodePairs(pairs [][2]string) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p[0]))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[1]))
	}
	return b.String()
}

// canonicalize re-serializes the query through a decode+encode round trip so
// Sign and Verify MAC the same byte representation (Rust canonicalize()).
func canonicalize(u *url.URL) *url.URL {
	c := *u
	c.RawQuery = encodePairs(queryPairs(u))
	return &c
}

// appendURLPath appends path onto u's existing path without discarding it
// (Rust signing::append_path) — e.g. a service URL carrying a
// "/notification" gateway prefix keeps it.
func appendURLPath(u *url.URL, path string) *url.URL {
	c := *u
	prefix := strings.TrimSuffix(u.Path, "/")
	suffix := strings.TrimPrefix(path, "/")
	c.Path = prefix + "/" + suffix
	return &c
}

// Sign canonicalizes u, computes the HMAC-SHA256 of its string form, and
// returns a copy of u with the hex signature appended as `sig`.
func (s *URLSigner) Sign(u *url.URL) (*url.URL, error) {
	if s == nil {
		return nil, fmt.Errorf("url signer not configured")
	}
	canonical := canonicalize(u)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(canonical.String()))
	signed := *canonical
	pairs := queryPairs(canonical)
	pairs = append(pairs, [2]string{sigParam, hex.EncodeToString(mac.Sum(nil))})
	signed.RawQuery = encodePairs(pairs)
	return &signed, nil
}

// Verify checks that u's `sig` parameter holds a valid HMAC over the
// canonical URL without `sig`. Exactly one sig parameter must be present
// (Rust partitions the query and requires a single sig pair).
func (s *URLSigner) Verify(u *url.URL) bool {
	if s == nil {
		return false
	}
	var sig string
	var others [][2]string
	sigCount := 0
	for _, p := range queryPairs(u) {
		if p[0] == sigParam {
			sigCount++
			sig = p[1]
			continue
		}
		others = append(others, p)
	}
	if sigCount != 1 {
		return false
	}
	expected, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	unsigned := *u
	unsigned.RawQuery = encodePairs(others)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(unsigned.String()))
	return subtle.ConstantTimeCompare(mac.Sum(nil), expected) == 1
}
