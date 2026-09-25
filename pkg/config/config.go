// Package config loads service configuration from environment variables.
// Replaces macro_env_var/macro_config + Doppler.
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

// Config is the root configuration shared by all subcommands.
// Per-subcommand structs embed it and add their own fields.
type Config struct {
	Env  string `env:"ENVIRONMENT" envDefault:"dev"`
	Port int    `env:"PORT" envDefault:"8080"`

	// Postgres. IMPORTANT: today this is ONE shared database — the Rust
	// services and the sqlc queries join across "domain" tables (e.g.
	// share_permission ⋈ comms_channels ⋈ email_threads), so all four URLs
	// must point at the same database until a real schema split exists.
	// The distinct vars exist so pools can be sized/monitored separately.
	MacroDBURL        string `env:"MACRO_DB_URL" envDefault:"postgres://macro:macro@localhost:5432/macrodb"`
	EmailDBURL        string `env:"EMAIL_DB_URL" envDefault:"postgres://macro:macro@localhost:5432/macrodb"`
	CommsDBURL        string `env:"COMMS_DB_URL" envDefault:"postgres://macro:macro@localhost:5432/macrodb"`
	NotificationDBURL string `env:"NOTIFICATION_DB_URL" envDefault:"postgres://macro:macro@localhost:5432/macrodb"`

	// NATS JetStream.
	NatsURL string `env:"NATS_URL" envDefault:"nats://localhost:4222"`

	// Valkey (Redis-compatible).
	ValkeyAddr string `env:"VALKEY_ADDR" envDefault:"localhost:6379"`

	// MinIO (S3-compatible).
	S3Endpoint        string `env:"S3_ENDPOINT" envDefault:"http://localhost:9000"`
	S3Region          string `env:"S3_REGION" envDefault:"us-east-1"`
	S3AccessKeyID     string `env:"S3_ACCESS_KEY_ID" envDefault:"minioadmin"`
	S3SecretAccessKey string `env:"S3_SECRET_ACCESS_KEY" envDefault:"minioadmin"`

	// Casdoor (replaces FusionAuth).
	CasdoorEndpoint     string `env:"CASDOOR_ENDPOINT" envDefault:"http://localhost:8000"`
	CasdoorClientID     string `env:"CASDOOR_CLIENT_ID" envDefault:""`
	CasdoorClientSecret string `env:"CASDOOR_CLIENT_SECRET" envDefault:""`
	CasdoorOrg          string `env:"CASDOOR_ORG" envDefault:"macro"`

	// SMTP (replaces SES send).
	SMTPHost string `env:"SMTP_HOST" envDefault:"localhost"`
	SMTPPort int    `env:"SMTP_PORT" envDefault:"1025"`
	SMTPUser string `env:"SMTP_USER" envDefault:""`
	SMTPPass string `env:"SMTP_PASS" envDefault:""`
	MailFrom string `env:"MAIL_FROM" envDefault:"macro@localhost"`
	// SMTPTLSPolicy: "opportunistic" (default), "mandatory", or "none".
	// Dev Mailhog listens plaintext on 1025 → opportunistic works there.
	SMTPTLSPolicy string `env:"SMTP_TLS_POLICY" envDefault:"opportunistic"`

	// OTel exporter (OTLP). Empty disables.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
}

// Load parses environment variables into a Config.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse env config: %w", err)
	}
	return c, nil
}
