package api

import (
	"fmt"

	"github.com/caarlos0/env/v11"
	"github.com/macro-inc/macro/pkg/config"
)

// Config extends the root config with env vars needed by the leaf services
// mounted under `macro api`.
type Config struct {
	config.Config

	// Internal service-to-service auth key (replaces macro_auth InternalApiKey).
	InternalAPIKey string `env:"INTERNAL_API_KEY" envDefault:""`

	// static_file_service
	StaticStorageBucket   string `env:"STATIC_STORAGE_BUCKET" envDefault:"macro-static"`
	StaticFileServiceURL  string `env:"STATIC_FILE_SERVICE_URL" envDefault:"http://localhost:8080"`
	StaticFileEventsTopic string `env:"S3_EVENTS_SUBJECT" envDefault:"s3.events"`

	// convert_service
	DocumentStorageBucket string `env:"DOCUMENT_STORAGE_BUCKET" envDefault:"macro-documents"`

	// document_storage_service — HS256 secret for document permission tokens
	// (Rust `document_permission_jwt`). Empty disables token minting.
	DocumentPermissionJWT string `env:"DOCUMENT_PERMISSION_JWT" envDefault:""`

	// email_service
	EmailAttachmentBucket  string `env:"EMAIL_ATTACHMENT_BUCKET" envDefault:"macro-email-attachments"`
	GoogleClientID         string `env:"GOOGLE_CLIENT_ID" envDefault:""`
	GoogleClientSecret     string `env:"GOOGLE_CLIENT_SECRET" envDefault:""`
	GmailPubsubTopic       string `env:"GMAIL_PUBSUB_TOPIC" envDefault:""`
	EmailSendUndoDelaySecs int    `env:"EMAIL_SEND_UNDO_DELAY_SECS" envDefault:"10"`
	EmailPresignGetSecs    int    `env:"EMAIL_PRESIGN_GET_SECS" envDefault:"3600"`
	// TokenEncryptionKey is a base64-encoded 32-byte AES-256 key encrypting
	// stored Gmail OAuth tokens at rest. Empty stores plaintext (warned).
	TokenEncryptionKey string `env:"TOKEN_ENCRYPTION_KEY" envDefault:""`
}

// Load parses environment variables into the api Config.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse api env config: %w", err)
	}
	return c, nil
}
