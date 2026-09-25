package syncsvc

import (
	"os"
	"strconv"
	"time"
)

// syncConfig holds env-driven knobs for the Go sync-service spike. It
// deliberately does NOT extend pkg/config.Config (out of scope); everything
// falls back to cfg.Port / cfg.MacroDBURL.
type syncConfig struct {
	// SYNC_PORT: HTTP listen port. Defaults to 8787 so `macro all` doesn't
	// collide with api:8080 / gateway:8085 / notification:8090.
	Port int
	// SYNC_INSECURE_AUTH: accept unauthenticated dev connections (port of the
	// Rust `insecure_auth` behavior).
	InsecureAuth bool
	// SYNC_INTERNAL_API_KEY (falls back to INTERNAL_API_KEY): value for
	// x-internal-auth-key (admin bypass, constant-time compared).
	InternalAPIKey string
	// SYNC_DOCUMENT_PERMISSION_JWT (falls back to DOCUMENT_PERMISSION_JWT):
	// HS256 secret verifying document-permission tokens minted by DSS —
	// the `document_permission_jwt` shared secret in the Rust config.
	PermissionJWTSecret string
	// SYNC_ALLOW_PASSTHROUGH: explicit opt-in to run without the loro wasm
	// engine. Without it (and without SYNC_INSECURE_AUTH) startup fails —
	// passthrough silently stores ops it cannot merge, which must never
	// look like a healthy sync service in production.
	AllowPassthrough bool
	// SYNC_WS_ORIGINS: comma list; empty or "*" accepts any ws origin.
	WSOrigins string
	// SYNC_LORO_WASM_PATH: path to loro_wasm_bg.wasm; empty = passthrough
	// engine (plumbing works, CRDT merge does not).
	LoroWasmPath string
	// SYNC_FLUSH_INTERVAL: pending-ops → snapshot flush period.
	FlushInterval time.Duration
	// SYNC_IDLE_TTL: how long an unlocked-but-socketless session stays warm.
	IdleTTL time.Duration
	// SYNC_PING_INTERVAL: ws keepalive ping period.
	PingInterval time.Duration
}

func loadSyncConfig() syncConfig {
	return syncConfig{
		Port:                envInt("SYNC_PORT", 8787),
		InsecureAuth:        envBool("SYNC_INSECURE_AUTH", false),
		InternalAPIKey:      firstEnv("SYNC_INTERNAL_API_KEY", "INTERNAL_API_KEY"),
		PermissionJWTSecret: firstEnv("SYNC_DOCUMENT_PERMISSION_JWT", "DOCUMENT_PERMISSION_JWT"),
		AllowPassthrough:    envBool("SYNC_ALLOW_PASSTHROUGH", false),
		WSOrigins:           os.Getenv("SYNC_WS_ORIGINS"),
		LoroWasmPath:        os.Getenv("SYNC_LORO_WASM_PATH"),
		FlushInterval:       envDur("SYNC_FLUSH_INTERVAL", 5*time.Minute),
		IdleTTL:             envDur("SYNC_IDLE_TTL", 60*time.Second),
		PingInterval:        envDur("SYNC_PING_INTERVAL", 30*time.Second),
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
