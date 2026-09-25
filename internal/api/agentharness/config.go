package agentharness

import (
	"fmt"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Config is the agentharness module's env config — the port of
// services/agent_harness_service/src/config.rs for the self-host build.
// Secret-bearing fields are resolved through runtime.SecretPort, not stored
// here; see secrets.go.
type Config struct {
	// BotID is the sandboxed-coding bot this deployment answers for
	// (HARNESS_BOT_ID). Empty leaves managed sessions unarmed: creation
	// fails loudly instead of silently impersonating a default bot.
	BotID string `env:"HARNESS_BOT_ID" envDefault:""`

	// Model is the model slug stamped onto managed sessions
	// (HARNESS_MODEL).
	Model string `env:"HARNESS_MODEL" envDefault:"claude"`

	// HarnessSlug is the harness slug stamped onto managed sessions
	// (HARNESS_SLUG).
	HarnessSlug string `env:"HARNESS_SLUG" envDefault:"opencode"`

	// RepoURL is the repository managed sessions clone through the egress
	// proxy (HARNESS_REPO_URL).
	RepoURL string `env:"HARNESS_REPO_URL" envDefault:"https://github.com/macro-inc/macro"`

	// RuntimeDriver selects the container runtime: "docker" for the
	// self-host Docker adapter, "none" to run unarmed (external sessions
	// still work; managed spawns fail at spawn time).
	RuntimeDriver string `env:"AGENT_RUNTIME_DRIVER" envDefault:"docker"`

	// DockerHost is the Docker daemon address (DOCKER_HOST /
	// AGENT_DOCKER_HOST), e.g. "unix:///var/run/docker.sock".
	DockerHost string `env:"AGENT_DOCKER_HOST" envDefault:""`

	// ContainerImage is the agent sandbox image
	// (LOCAL_CONTAINER_IMAGE equivalent).
	ContainerImage string `env:"AGENT_CONTAINER_IMAGE" envDefault:"macro-agent-harness:latest"`

	// ContainerNetwork is the Docker network sandboxes join so this service
	// can dial them by name (LOCAL_CONTAINER_NETWORK equivalent). Required
	// when RuntimeDriver=docker and this process is itself a container.
	ContainerNetwork string `env:"AGENT_CONTAINER_NETWORK" envDefault:""`

	// EnvAllowlist is the comma-separated list of env keys permitted into
	// sandbox containers. Everything else in a spawn spec is dropped.
	EnvAllowlist string `env:"AGENT_CONTAINER_ENV_ALLOWLIST" envDefault:"ANTHROPIC_API_KEY,MACRO_SESSION_TOKEN,MACRO_EGRESS_URL"`

	// SecretPrefix namespaces env-resolved secrets (AGENT_SECRET_<NAME>).
	SecretPrefix string `env:"AGENT_SECRET_PREFIX" envDefault:"AGENT_SECRET_"`

	// PullImages pulls sandbox images missing on the daemon.
	PullImages bool `env:"AGENT_CONTAINER_PULL" envDefault:"false"`

	// EgressURL is the base URL sandboxes dial for the egress proxy
	// (MACRO_EGRESS_URL). Empty disables git/MCP egress for sandboxes;
	// sessions still run but cannot clone through the proxy.
	// TODO(egress): port the egress proxy (agent_egress) and point this at it.
	EgressURL string `env:"AGENT_EGRESS_URL" envDefault:""`

	// TriggerEnabled runs the in-process agent_trigger consumer.
	TriggerEnabled bool `env:"AGENT_TRIGGER_ENABLED" envDefault:"true"`

	// TriggerConsumer is the durable consumer name on the agent.trigger
	// stream.
	TriggerConsumer string `env:"AGENT_TRIGGER_CONSUMER" envDefault:"agent-harness"`
}

// Load parses environment variables into a Config.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse agentharness env config: %w", err)
	}
	return c, nil
}

// allowlist parses EnvAllowlist into a runtime.EnvAllowlist.
func (c Config) allowlist() []string {
	var out []string
	for _, k := range strings.Split(c.EnvAllowlist, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}
