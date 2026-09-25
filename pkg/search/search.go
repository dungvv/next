// Package search defines the search port that replaces OpenSearch in the
// self-hosted build: keyword full-text search over a Postgres tsvector index
// plus optional pgvector semantic/hybrid ranking (docs/GO_SELFHOST_PLAN.md).
package search

import (
	"context"
	"errors"
	"time"
)

// Entity types indexed by the search processing pipeline. They mirror the
// OpenSearch index families the Rust service wrote to.
const (
	EntityDocument       = "document"
	EntityChatMessage    = "chat_message"
	EntityChannelMessage = "channel_message"
	EntityEmailThread    = "email_thread"
	EntityCall           = "call"
	EntityProject        = "project"
	EntityCalendarEvent  = "calendar_event"
	EntityUser           = "user"
)

// Document is one indexable entity. EntityID + EntityType are the primary
// key; Content feeds the tsvector; Embedding feeds pgvector when enabled.
type Document struct {
	EntityID   string         `json:"entity_id"`
	EntityType string         `json:"entity_type"`
	OwnerID    string         `json:"owner_id"`
	Title      string         `json:"title"`
	Content    string         `json:"content"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Embedding  []float32      `json:"-"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// Mode selects the ranking path.
type Mode int

const (
	// ModeKeyword runs tsvector @@ websearch_to_tsquery ranking.
	ModeKeyword Mode = iota
	// ModeSemantic ranks by pgvector cosine distance (requires Embedding).
	ModeSemantic
	// ModeHybrid blends ts_rank and cosine similarity.
	ModeHybrid
)

// Query is one search request against the index.
type Query struct {
	Text        string
	Embedding   []float32
	OwnerID     string   // empty = all owners (internal callers filter themselves)
	EntityTypes []string // empty = every type
	Mode        Mode
	Limit       int
	// HybridWeight blends scores in ModeHybrid: weight on the FTS rank,
	// remainder on vector similarity (default 0.5).
	HybridWeight float64
}

// Hit is one scored result.
type Hit struct {
	EntityID   string `json:"entity_id"`
	EntityType string `json:"entity_type"`
	OwnerID    string `json:"owner_id"`
	Title      string `json:"title"`
	// Snippet is a ts_headline excerpt with match markers (<b>…</b>).
	Snippet   string         `json:"snippet"`
	Score     float64        `json:"score"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// SearchPort is the outbound contract the API/worker side talks to. The
// Postgres adapter (PgStore) is the production implementation.
type SearchPort interface {
	// Index upserts documents by (entity_type, entity_id).
	Index(ctx context.Context, docs []Document) error
	// MergeMetadata JSONB-merges patch into an existing document's metadata
	// (used by the property reindex path, which must not clobber
	// title/content).
	MergeMetadata(ctx context.Context, entityType, entityID string, patch map[string]any) error
	// Delete removes one entity from the index.
	Delete(ctx context.Context, entityType, entityID string) error
	// Search runs a keyword/semantic/hybrid query.
	Search(ctx context.Context, q Query) ([]Hit, error)
}

// Embedder produces dense vectors for semantic search (replaces the
// OpenSearch knn/embedding pipeline). Wired in when an embedding provider is
// configured; NoopEmbedder reports unavailability so callers can fall back
// to keyword mode.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ErrEmbeddingsUnavailable is returned when no embedding provider is
// configured.
var ErrEmbeddingsUnavailable = errors.New("search: embedding provider not configured")

// NoopEmbedder reports embeddings as unavailable.
type NoopEmbedder struct{}

// Embed implements Embedder.
func (NoopEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, ErrEmbeddingsUnavailable
}
