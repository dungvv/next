package gateway

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/macro-inc/macro/pkg/config"
)

// Config holds gateway-specific settings layered on top of the shared
// config.Config (which supplies NATS, Valkey and Postgres coordinates).
//
// Port of services/connection_gateway/src/config.rs: the Rust service read
// `port`, `redis_host`, `macro_db_url` and `internal_api_key`; those map to
// GATEWAY_PORT, the shared VALKEY_ADDR / MACRO_DB_URL, and
// GATEWAY_INTERNAL_API_KEY here.
type Config struct {
	// Port for the public websocket endpoint. Defaults to 8085 (the Rust
	// connection_gateway port) so `macro all` doesn't collide with api:8080.
	Port int `env:"GATEWAY_PORT" envDefault:"8085"`
	// InternalPort serves the service-to-service API (/internal/* plus the
	// connection_gateway_client-compatible routes). Default 8086.
	InternalPort int `env:"GATEWAY_INTERNAL_PORT" envDefault:"8086"`

	// JWTSecret is the HMAC secret used to verify client JWTs.
	JWTSecret string `env:"GATEWAY_JWT_SECRET"`
	// JWKSURL, when set, selects a JWKS-backed verifier (placeholder, see
	// auth.go). Takes precedence over JWTSecret.
	JWKSURL string `env:"GATEWAY_JWKS_URL"`
	// InsecureAuth disables signature verification. Development only.
	InsecureAuth bool `env:"GATEWAY_INSECURE_AUTH" envDefault:"false"`

	// InternalAPIKey guards the internal HTTP API via the
	// `x-internal-auth-key` header (same header the Rust
	// connection_gateway_client sends). Empty means open — dev only.
	// Falls back to INTERNAL_API_KEY for parity with the Rust config.
	InternalAPIKey string `env:"GATEWAY_INTERNAL_API_KEY"`

	// Env is the shared ENVIRONMENT value ("dev", "staging", "prod").
	// When InternalAPIKey is empty, the internal API only stays open in
	// dev; every other environment fails closed.
	Env string

	// WSAllowedOrigins is a comma-separated list of allowed Origin host
	// patterns for the websocket endpoint (path.Match syntax; may include
	// scheme://host). Empty accepts every origin — dev posture, tighten via
	// env in production. Replaces the static ORIGINS list in constants.rs.
	WSAllowedOrigins string `env:"GATEWAY_WS_ORIGINS"`

	// ConnTTL is the registry heartbeat TTL and the stale-connection
	// threshold. Port of DEFAULT_TIMEOUT_THRESHOLD (60s).
	ConnTTL time.Duration `env:"GATEWAY_CONN_TTL" envDefault:"60s"`
	// SweepInterval is how often the stale-connection sweeper runs
	// (port of scripts/stale_connections.rs run as a cron).
	SweepInterval time.Duration `env:"GATEWAY_SWEEP_INTERVAL" envDefault:"30s"`
	// SendBuffer is the per-connection outbound queue depth. A full queue
	// drops the connection, matching the Rust mpsc channel of 100 → try_send.
	SendBuffer int `env:"GATEWAY_SEND_BUFFER" envDefault:"256"`
	// WriteTimeout bounds a single websocket write.
	WriteTimeout time.Duration `env:"GATEWAY_WRITE_TIMEOUT" envDefault:"10s"`
}

// loadConfig parses gateway env vars and folds in the shared config.
func loadConfig(root config.Config) (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse gateway env config: %w", err)
	}
	c.Env = root.Env
	if c.InternalAPIKey == "" {
		// Rust used INTERNAL_API_KEY; accept it as a fallback.
		c.InternalAPIKey = envOr("INTERNAL_API_KEY", "")
	}
	if c.Port == 0 {
		c.Port = root.Port
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// wsOriginPatterns splits WSAllowedOrigins into coder/websocket patterns.
func (c Config) wsOriginPatterns() []string {
	if strings.TrimSpace(c.WSAllowedOrigins) == "" {
		return nil
	}
	parts := strings.Split(c.WSAllowedOrigins, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
