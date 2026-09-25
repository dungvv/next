package runtime

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// SecretPort resolves named secrets into values for injection into agent
// task environments. It replaces the Rust SecretsManager/Doppler lookups.
//
// The env adapter below is the self-host baseline: secrets are ordinary
// process environment variables. TODO(secrets): add sops- or
// Infisical-backed adapters; keep resolution behind this interface so the
// container path never sees where a secret came from.
type SecretPort interface {
	// Resolve returns the secret value for name, or an error when unset.
	Resolve(ctx context.Context, name string) (string, error)
}

// EnvSecrets resolves secrets from process environment variables. A request
// for name "ANTHROPIC_API_KEY" reads env var "<Prefix>ANTHROPIC_API_KEY"
// first, then the bare name — so the module accepts both a dedicated secret
// namespace (e.g. AGENT_SECRET_ANTHROPIC_API_KEY) and the plain service env
// the Rust config used.
type EnvSecrets struct {
	// Prefix is prepended to the requested name before lookup; empty means
	// "read the bare name only". Example: "AGENT_SECRET_".
	Prefix string
}

// Resolve implements SecretPort.
func (s EnvSecrets) Resolve(_ context.Context, name string) (string, error) {
	if s.Prefix != "" {
		if v := os.Getenv(s.Prefix + name); v != "" {
			return v, nil
		}
	}
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("secret %q is not set", name)
}

// NoopSecrets resolves nothing. Useful when the runtime is unarmed: managed
// spawns fail loudly at Resolve time rather than silently starting a
// credential-less agent.
type NoopSecrets struct{}

// Resolve implements SecretPort.
func (NoopSecrets) Resolve(_ context.Context, name string) (string, error) {
	return "", fmt.Errorf("secret %q requested but no secret store is configured", name)
}

// EnvAllowlist gates which environment variables may be injected into an
// agent task. Task environments carry model credentials and session tokens —
// the sandbox runs model-authored code, so the allowlist is the boundary
// that keeps an arbitrary caller from exfiltrating host env into a sandbox.
type EnvAllowlist map[string]struct{}

// NewEnvAllowlist builds an allowlist from exact names. Names are
// case-sensitive; "FOO_*" style suffix wildcards are supported.
func NewEnvAllowlist(names ...string) EnvAllowlist {
	set := make(EnvAllowlist, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// Allows reports whether key may be injected.
func (a EnvAllowlist) Allows(key string) bool {
	if _, ok := a[key]; ok {
		return true
	}
	for name := range a {
		if strings.HasSuffix(name, "*") && strings.HasPrefix(key, strings.TrimSuffix(name, "*")) {
			return true
		}
	}
	return false
}

// Filter drops disallowed keys, returning the surviving entries in
// unspecified order plus the dropped keys for logging.
func (a EnvAllowlist) Filter(env map[string]string) (allowed map[string]string, dropped []string) {
	allowed = make(map[string]string, len(env))
	for k, v := range env {
		if a.Allows(k) {
			allowed[k] = v
		} else {
			dropped = append(dropped, k)
		}
	}
	return allowed, dropped
}
