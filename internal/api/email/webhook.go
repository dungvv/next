package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"

	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/store/emaildb"
)

// ---------------------------------------------------------------------------
// Google Pub/Sub push authentication
// (ports validate_google_token + gmail_client::verify_google_jwt)
// ---------------------------------------------------------------------------

// googleCertsURL is Google's OIDC JWKS endpoint (certs_url in the Rust
// gmail_client). Overridable via GMAIL_PUBSUB_JWKS_URL for tests.
const googleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

// googleIssuers are the issuer spellings Google uses on its OIDC tokens
// (validation.set_issuer in verify_google_jwt).
var googleIssuers = map[string]struct{}{
	"accounts.google.com":         {},
	"https://accounts.google.com": {},
}

var (
	pubsubAuthOnce sync.Once
	pubsubVerifier *oidc.IDTokenVerifier
	pubsubAuthErr  error
)

// isDevEnv reports whether ENVIRONMENT names a local/dev deployment — the
// webhook tolerates an unconfigured audience there only.
func isDevEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ENVIRONMENT"))) {
	case "", "dev", "develop", "development", "local", "test":
		return true
	default:
		return false
	}
}

// pubsubOIDCVerifier lazily builds the Google-token verifier from env:
//
//	GMAIL_PUBSUB_OIDC_AUDIENCE — expected `aud` claim (the Pub/Sub push
//	  subscription's audience; the Rust client used "macro-gmail-webhook").
//	  Required outside dev: unconfigured → every push is rejected.
//	GMAIL_PUBSUB_JWKS_URL — JWKS endpoint override (defaults to Google's
//	  certs endpoint; tests point it at a stub).
//
// Key rotation is handled by the oidc remote key set's own caching, which
// replaces the Rust Redis-cached get/fetch_and_refresh pair.
func pubsubOIDCVerifier() (*oidc.IDTokenVerifier, error) {
	pubsubAuthOnce.Do(func() {
		audience := strings.TrimSpace(os.Getenv("GMAIL_PUBSUB_OIDC_AUDIENCE"))
		if audience == "" && !isDevEnv() {
			pubsubAuthErr = errors.New(
				"GMAIL_PUBSUB_OIDC_AUDIENCE is required outside dev environments")
			return
		}
		if audience == "" {
			slog.Warn("email: GMAIL_PUBSUB_OIDC_AUDIENCE unset — gmail webhook verifies the Google signature and issuer but skips the audience check (dev only)")
		}
		jwksURL := strings.TrimSpace(os.Getenv("GMAIL_PUBSUB_JWKS_URL"))
		if jwksURL == "" {
			jwksURL = googleCertsURL
		}
		cfg := &oidc.Config{SkipIssuerCheck: true} // iss allowlist checked below
		if audience == "" {
			cfg.SkipClientIDCheck = true
		} else {
			cfg.ClientID = audience
		}
		pubsubVerifier = oidc.NewVerifier("",
			oidc.NewRemoteKeySet(context.Background(), jwksURL), cfg)
	})
	return pubsubVerifier, pubsubAuthErr
}

// errPubsubNoToken is the 401 for a missing bearer token.
var errPubsubNoToken = errors.New("missing Authorization bearer token")

// verifyPubsubBearer validates the Google OIDC bearer token Pub/Sub attaches
// to push deliveries (see
// https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions):
// RS256 signature against Google's JWKS, issuer accounts.google.com, and the
// configured audience.
func verifyPubsubBearer(r *http.Request) error {
	verifier, err := pubsubOIDCVerifier()
	if err != nil {
		return err
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return errPubsubNoToken
	}
	tok, err := verifier.Verify(r.Context(), token)
	if err != nil {
		return fmt.Errorf("invalid authentication token: %w", err)
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := tok.Claims(&claims); err != nil {
		return fmt.Errorf("invalid authentication token: %w", err)
	}
	if _, ok := googleIssuers[claims.Issuer]; !ok {
		return fmt.Errorf("invalid authentication token: unexpected issuer %q", claims.Issuer)
	}
	return nil
}

// gmailWebhook handles POST /gmail/webhook — Google Pub/Sub push receiver.
// The bearer token is verified against Google's OIDC JWKS (the Rust
// gmail_webhook_auth feature check); then the payload shape is validated and
// the pushed emailAddress is mapped to a link before enqueueing an inbox
// refresh. Always returns 204 for malformed-but-well-formed-envelope pushes
// so Pub/Sub doesn't retry poison messages forever.
func (d *Deps) gmailWebhook(w http.ResponseWriter, r *http.Request) {
	if err := verifyPubsubBearer(r); err != nil {
		if errors.Is(err, errPubsubNoToken) {
			slog.Warn("email: gmail webhook missing bearer token")
			httpx.ErrorJSON(w, http.StatusUnauthorized, "Missing Authorization bearer token")
			return
		}
		if pubsubAuthErr != nil && errors.Is(err, pubsubAuthErr) {
			slog.Error("email: gmail webhook auth not configured", "err", err)
			httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to get google public keys")
			return
		}
		slog.Warn("email: gmail webhook token verification failed", "err", err)
		httpx.ErrorJSON(w, http.StatusUnauthorized, "Invalid authentication token")
		return
	}
	var env pubsubEnvelope
	if !httpx.DecodeJSON(w, r, &env) {
		return
	}
	if len(env.Message.Data) == 0 {
		// Pub/Sub verification pings carry no data — ack and move on.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var push gmailPushData
	if err := json.Unmarshal(env.Message.Data, &push); err != nil || push.EmailAddress == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx := r.Context()
	link, err := d.q().FetchLinkByEmail(ctx, emaildb.FetchLinkByEmailParams{
		EmailAddress: push.EmailAddress,
		Provider:     emaildb.EmailUserProviderEnumGMAIL,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown mailbox — ack to stop retries.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to look up link")
		return
	}
	if !link.IsSyncActive {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := d.publishJob(ctx, subjEmailRefresh, "email.refresh", RefreshInboxJob{
		LinkID:       uuidFromPg(link.ID),
		EmailAddress: push.EmailAddress,
		HistoryID:    push.HistoryID,
	}); err != nil {
		// 503 → Pub/Sub retries, which is what we want on queue outages.
		httpx.ErrorJSON(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
