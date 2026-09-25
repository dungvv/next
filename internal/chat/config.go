package chat

import (
	"fmt"

	"github.com/caarlos0/env/v11"
	"github.com/macro-inc/macro/pkg/config"
)

// Config is the chat service configuration.
type Config struct {
	config.Config

	// Port is the HTTP listen port (env CHAT_PORT). Defaults to 8095 so
	// `macro all` doesn't collide with api:8080 / gateway:8085 /
	// notification:8090 / sync:8787.
	Port int `env:"CHAT_PORT" envDefault:"8095"`

	// InternalAPIKey authenticates service-to-service callers
	// (x-internal-auth-key header).
	InternalAPIKey string `env:"INTERNAL_API_KEY"`
}

// LoadConfig parses the shared + chat-specific environment.
func LoadConfig() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse chat config: %w", err)
	}
	return c, nil
}
