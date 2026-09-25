package dcs

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

// Config holds the document cognition service settings. The AI provider
// credentials live here rather than the shared api.Config so the api package
// stays free of leaf-service concerns.
type Config struct {
	// Anthropic is the primary chat-completion provider
	// (replaces the rig anthropic client in the Rust agent crate).
	AnthropicAPIKey  string `env:"ANTHROPIC_API_KEY" envDefault:""`
	AnthropicBaseURL string `env:"ANTHROPIC_BASE_URL" envDefault:"https://api.anthropic.com"`
	AnthropicVersion string `env:"ANTHROPIC_VERSION" envDefault:"2023-06-01"`
	// MaxTokens caps Anthropic completions (agent DEFAULT_MAX_TOKENS = 16_000).
	AnthropicMaxTokens int `env:"ANTHROPIC_MAX_TOKENS" envDefault:"16000"`

	// OpenAI backs POST /chat/completions (an OpenAI-compatible proxy).
	OpenAIAPIKey  string `env:"OPENAI_API_KEY" envDefault:""`
	OpenAIBaseURL string `env:"OPENAI_BASE_URL" envDefault:"https://api.openai.com"`

	// StreamCancelSubject is the NATS subject prefix used to broadcast
	// cross-instance stream cancellations: "<prefix>.<stream_id>".
	// Empty disables cross-instance stop (local registry still works).
	StreamCancelSubject string `env:"DCS_STREAM_CANCEL_SUBJECT" envDefault:"dcs.stream.cancel"`

	// IdleTimeoutSeconds bounds how long an AI stream may go without a token
	// before it is aborted (Rust uses 3 minutes).
	StreamIdleTimeoutSeconds int `env:"DCS_STREAM_IDLE_TIMEOUT_SECONDS" envDefault:"180"`
}

// Load parses environment variables into the dcs Config.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse dcs env config: %w", err)
	}
	return c, nil
}
