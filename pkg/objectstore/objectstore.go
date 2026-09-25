// Package objectstore defines the object-storage port used by services that
// read/write blobs (static files, document payloads, uploads). The default
// adapter is S3-compatible and targets MinIO in the self-host deployment
// (any S3-compatible endpoint works via S3_ENDPOINT).
package objectstore

import (
	"context"
	"io"
	"time"
)

// Store is the object-storage port. Implementations are S3-compatible.
type Store interface {
	// GetObject returns the object's body and metadata. The caller must close
	// the returned body.
	GetObject(ctx context.Context, bucket, key string) (*Object, error)
	// PutObject uploads a body to bucket/key.
	PutObject(ctx context.Context, bucket, key string, body io.Reader, contentType string) error
	// DeleteObject removes a single object. Deleting a missing key is not an
	// error on S3-compatible stores.
	DeleteObject(ctx context.Context, bucket, key string) error
	// DeleteObjects removes up to 1000 keys per request and returns one
	// result per input key, in input order.
	DeleteObjects(ctx context.Context, bucket string, keys []string) []error
	// DeleteByPrefix lists and deletes every object under prefix.
	DeleteByPrefix(ctx context.Context, bucket, prefix string) error
	// Exists reports whether bucket/key exists.
	Exists(ctx context.Context, bucket, key string) (bool, error)
	// PresignGet returns an expiring GET URL for bucket/key.
	PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)
	// PresignPut returns an expiring PUT URL for bucket/key that asserts the
	// given content type.
	PresignPut(ctx context.Context, bucket, key, contentType string, ttl time.Duration) (string, error)
}

// Object is a fetched blob plus the response metadata services care about.
type Object struct {
	Body        io.ReadCloser
	ContentType string
	// ContentLength is the object size, or -1 when unknown.
	ContentLength int64
	ETag          string
}

// Config configures the S3-compatible adapter.
type Config struct {
	// Endpoint overrides the S3 endpoint (e.g. http://localhost:9000 for
	// MinIO). Empty means real AWS.
	Endpoint string
	Region   string
	// Static credentials. Leave empty to use the default AWS credential
	// chain (instance role, env vars, etc.).
	AccessKeyID     string
	SecretAccessKey string
	// UsePathStyle forces path-style (endpoint/bucket/key) requests.
	// Required by MinIO; defaults to true when Endpoint is set.
	UsePathStyle bool
}
