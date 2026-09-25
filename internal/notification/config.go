package notification

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/macro-inc/macro/pkg/config"
)

// Config is the notification service configuration: the shared root config
// plus notification-specific env vars.
type Config struct {
	config.Config

	// Port is the HTTP listen port (env NOTIFICATION_PORT). Defaults to 8090
	// so `macro all` doesn't collide with api:8080 / gateway:8085.
	Port int `env:"NOTIFICATION_PORT" envDefault:"8090"`

	// GatewayInternalURL is the gateway's internal API base
	// (env GATEWAY_INTERNAL_URL). Delivery POSTs to <url>/internal/send.
	GatewayInternalURL string `env:"GATEWAY_INTERNAL_URL" envDefault:"http://localhost:8086"`

	// InternalAPIKey authenticates calls to the gateway internal API and
	// internal callers of this service (x-internal-auth-key header).
	InternalAPIKey string `env:"INTERNAL_API_KEY"`

	// URLSigningHMAC is the hex/raw secret for unsubscribe presigned URLs.
	URLSigningHMAC string `env:"URL_SIGNING_HMAC"`

	// NotificationServiceURL is the public base URL of this service, used to
	// build/verify presigned links (env NOTIFICATION_SERVICE_URL).
	NotificationServiceURL string `env:"NOTIFICATION_SERVICE_URL" envDefault:"http://localhost:8080"`

	// FCM push (HTTP v1). Empty disables the FCM adapter.
	FCMServiceAccountJSON string `env:"FCM_SERVICE_ACCOUNT_JSON"`
	// FCMEndpoint overrides the FCM base URL (tests).
	FCMEndpoint string `env:"FCM_ENDPOINT"`

	// APNs push (token auth). All four are required to enable the adapter.
	APNSKeyFile      string `env:"APNS_KEY_FILE"`
	APNSKeyID        string `env:"APNS_KEY_ID"`
	APNSTeamID       string `env:"APNS_TEAM_ID"`
	APNSBundleID     string `env:"APNS_BUNDLE_ID"`
	APNSVoIPBundleID string `env:"APNS_VOIP_BUNDLE_ID"`
	APNSSandbox      bool   `env:"APNS_SANDBOX"`

	// DigestWindow is how long failed-push notifications collect per user
	// before a digest email is sent (env NOTIFICATION_DIGEST_WINDOW).
	// Rust StateMachineDriverA uses 24h.
	DigestWindow time.Duration `env:"NOTIFICATION_DIGEST_WINDOW" envDefault:"24h"`
	// DigestEgressWindow is the window used when a digest is queued on the
	// egress side — all push endpoints failed (Rust StateMachineDriverB) or
	// push-event reconciliation marked every receipt failed (DriverC). Rust
	// uses 30m for both (env NOTIFICATION_DIGEST_EGRESS_WINDOW).
	DigestEgressWindow time.Duration `env:"NOTIFICATION_DIGEST_EGRESS_WINDOW" envDefault:"30m"`
	// DigestOnlineThreshold: a user with push notifications disabled who was
	// online within this window does not get digest email
	// (Rust online_duration_threshold = 60min, env
	// NOTIFICATION_DIGEST_ONLINE_THRESHOLD).
	DigestOnlineThreshold time.Duration `env:"NOTIFICATION_DIGEST_ONLINE_THRESHOLD" envDefault:"60m"`
	// DigestPollInterval is how often the digest flusher polls for ready
	// batches (env NOTIFICATION_DIGEST_POLL_INTERVAL).
	DigestPollInterval time.Duration `env:"NOTIFICATION_DIGEST_POLL_INTERVAL" envDefault:"10s"`
	// DigestStalenessThreshold discards digests older than this
	// (env NOTIFICATION_DIGEST_STALENESS_THRESHOLD).
	DigestStalenessThreshold time.Duration `env:"NOTIFICATION_DIGEST_STALENESS_THRESHOLD" envDefault:"48h"`

	// SMTPTLSPolicy: "mandatory" (default), "opportunistic", or "none"
	// (env SMTP_TLS_POLICY — use "none" for mailhog).
	SMTPTLSPolicy string `env:"SMTP_TLS_POLICY" envDefault:"none"`

	// MailFrom overrides config.MailFrom for notification emails.
	MailFrom string `env:"NOTIFICATION_MAIL_FROM"`
}

// LoadConfig parses the shared config plus notification env vars.
func LoadConfig() (Config, error) {
	root, err := config.Load()
	if err != nil {
		return Config{}, err
	}
	return fromRoot(root)
}

// fromRoot extends a parsed root config with the notification env vars.
func fromRoot(root config.Config) (Config, error) {
	c := Config{Config: root}
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse notification config: %w", err)
	}
	if c.Port == 0 {
		c.Port = root.Port
	}
	if c.MailFrom == "" {
		c.MailFrom = root.MailFrom
	}
	return c, nil
}
