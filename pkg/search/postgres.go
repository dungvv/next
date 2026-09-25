package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTable is the index table EnsureSchema creates lazily.
const DefaultTable = "search_index"

// PgStore is the Postgres SearchPort adapter: a generated tsvector column
// (GIN-indexed) backs keyword search; an optional pgvector `embedding`
// column backs semantic/hybrid search.
type PgStore struct {
	db    *pgxpool.Pool
	table string
	dims  int // 0 = pgvector disabled
}

// PgOption configures a PgStore.
type PgOption func(*PgStore)

// WithTable overrides the index table name.
func WithTable(table string) PgOption {
	return func(s *PgStore) { s.table = table }
}

// WithVectorDimensions enables the pgvector embedding column of the given
// dimension (e.g. 1536). Requires the `vector` extension; EnsureSchema
// creates it best-effort.
func WithVectorDimensions(dims int) PgOption {
	return func(s *PgStore) { s.dims = dims }
}

// NewPgStore builds the adapter over a macrodb pool.
func NewPgStore(db *pgxpool.Pool, opts ...PgOption) *PgStore {
	s := &PgStore{db: db, table: DefaultTable}
	for _, o := range opts {
		o(s)
	}
	return s
}

// EnsureSchema creates the index table and indexes when missing. Replaces
// the OpenSearch index bootstrap.
// TODO(migrate): move this into crates/macro_db_client/migrations like the
// static_files table.
func (s *PgStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			entity_type TEXT        NOT NULL,
			entity_id   TEXT        NOT NULL,
			owner_id    TEXT        NOT NULL DEFAULT '',
			title       TEXT        NOT NULL DEFAULT '',
			content     TEXT        NOT NULL DEFAULT '',
			metadata    JSONB       NOT NULL DEFAULT '{}',
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			tsv         TSVECTOR    GENERATED ALWAYS AS (
				setweight(to_tsvector('english', title), 'A') ||
				setweight(to_tsvector('english', content), 'B')
			) STORED,
			PRIMARY KEY (entity_type, entity_id)
		)`, s.table)); err != nil {
		return fmt.Errorf("search: create table: %w", err)
	}
	if _, err := s.db.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_tsv ON %s USING gin(tsv)`,
		s.table, s.table)); err != nil {
		return fmt.Errorf("search: create tsv index: %w", err)
	}
	if _, err := s.db.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_owner ON %s (owner_id, entity_type, updated_at DESC)`,
		s.table, s.table)); err != nil {
		return fmt.Errorf("search: create owner index: %w", err)
	}
	if s.dims > 0 {
		if err := s.ensureVector(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ensureVector adds the pgvector column + HNSW index. Fails loudly so a
// misconfigured deployment surfaces at boot instead of silently degrading
// to keyword-only.
func (s *PgStore) ensureVector(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("search: create vector extension: %w", err)
	}
	if _, err := s.db.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS embedding vector(%d)`,
		s.table, s.dims)); err != nil {
		return fmt.Errorf("search: add embedding column: %w", err)
	}
	if _, err := s.db.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_embedding ON %s USING hnsw (embedding vector_cosine_ops)`,
		s.table, s.table)); err != nil {
		return fmt.Errorf("search: create embedding index: %w", err)
	}
	return nil
}

// encodeVector renders a float32 slice as a pgvector literal '[…]'.
func encodeVector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// Index implements SearchPort.
func (s *PgStore) Index(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}
	query := fmt.Sprintf(`
		INSERT INTO %s (entity_type, entity_id, owner_id, title, content, metadata, embedding, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (entity_type, entity_id) DO UPDATE SET
			owner_id   = EXCLUDED.owner_id,
			title      = EXCLUDED.title,
			content    = EXCLUDED.content,
			metadata   = EXCLUDED.metadata,
			embedding  = COALESCE(EXCLUDED.embedding, %s.embedding),
			updated_at = EXCLUDED.updated_at`, s.table, s.table)
	if s.dims == 0 {
		// No embedding column: NULL out the vector parameter.
		query = fmt.Sprintf(`
		INSERT INTO %s (entity_type, entity_id, owner_id, title, content, metadata, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (entity_type, entity_id) DO UPDATE SET
			owner_id   = EXCLUDED.owner_id,
			title      = EXCLUDED.title,
			content    = EXCLUDED.content,
			metadata   = EXCLUDED.metadata,
			updated_at = EXCLUDED.updated_at`, s.table)
	}
	batch := &pgx.Batch{}
	for _, d := range docs {
		meta, err := json.Marshal(d.Metadata)
		if err != nil {
			return fmt.Errorf("search: marshal metadata: %w", err)
		}
		ts := d.UpdatedAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if s.dims == 0 {
			batch.Queue(query, d.EntityType, d.EntityID, d.OwnerID, d.Title, d.Content, meta, ts)
		} else {
			var emb *string
			if len(d.Embedding) > 0 {
				e := encodeVector(d.Embedding)
				emb = &e
			}
			batch.Queue(query, d.EntityType, d.EntityID, d.OwnerID, d.Title, d.Content, meta, emb, ts)
		}
	}
	return s.db.SendBatch(ctx, batch).Close()
}

// MergeMetadata implements SearchPort.
func (s *PgStore) MergeMetadata(ctx context.Context, entityType, entityID string, patch map[string]any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET metadata = metadata || $3::jsonb, updated_at = now()
		 WHERE entity_type = $1 AND entity_id = $2`, s.table),
		entityType, entityID, raw)
	return err
}

// Delete implements SearchPort.
func (s *PgStore) Delete(ctx context.Context, entityType, entityID string) error {
	_, err := s.db.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE entity_type = $1 AND entity_id = $2`, s.table),
		entityType, entityID)
	return err
}

// Search implements SearchPort.
func (s *PgStore) Search(ctx context.Context, q Query) ([]Hit, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	w := q.HybridWeight
	if w <= 0 || w > 1 {
		w = 0.5
	}

	var (
		sql  string
		args []any
	)
	switch q.Mode {
	case ModeSemantic:
		if len(q.Embedding) == 0 || s.dims == 0 {
			return nil, fmt.Errorf("search: semantic mode requires pgvector + query embedding")
		}
		args = append(args, encodeVector(q.Embedding))
		sql = fmt.Sprintf(`
			SELECT entity_type, entity_id, owner_id, title,
			       '' AS snippet, 1 - (embedding <=> $1::vector) AS score,
			       metadata, updated_at
			FROM %s
			WHERE embedding IS NOT NULL`, s.table)
	case ModeHybrid:
		if len(q.Embedding) == 0 || s.dims == 0 {
			// No vectors available: degrade to keyword.
			q.Mode = ModeKeyword
		} else {
			args = append(args, q.Text, encodeVector(q.Embedding), w)
			sql = fmt.Sprintf(`
				SELECT entity_type, entity_id, owner_id, title,
				       ts_headline('english', content, websearch_to_tsquery('english', $1),
				                   'MaxWords=30,MinWords=15,StartSel=<b>,StopSel=</b>') AS snippet,
				       ts_rank(tsv, websearch_to_tsquery('english', $1)) * $3
				         + (1 - (embedding <=> $2::vector)) * (1 - $3) AS score,
				       metadata, updated_at
				FROM %s
				WHERE (tsv @@ websearch_to_tsquery('english', $1)
				       OR embedding IS NOT NULL)`, s.table)
		}
	}
	if q.Mode == ModeKeyword {
		args = append(args, q.Text)
		sql = fmt.Sprintf(`
			SELECT entity_type, entity_id, owner_id, title,
			       ts_headline('english', content, websearch_to_tsquery('english', $1),
			                   'MaxWords=30,MinWords=15,StartSel=<b>,StopSel=</b>') AS snippet,
			       ts_rank(tsv, websearch_to_tsquery('english', $1)) AS score,
			       metadata, updated_at
			FROM %s
			WHERE tsv @@ websearch_to_tsquery('english', $1)`, s.table)
	}

	if q.OwnerID != "" {
		args = append(args, q.OwnerID)
		sql += fmt.Sprintf(" AND owner_id = $%d", len(args))
	}
	if len(q.EntityTypes) > 0 {
		args = append(args, q.EntityTypes)
		sql += fmt.Sprintf(" AND entity_type = ANY($%d)", len(args))
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY score DESC LIMIT $%d", len(args))

	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search: query: %w", err)
	}
	defer rows.Close()

	var hits []Hit
	for rows.Next() {
		var h Hit
		var meta []byte
		if err := rows.Scan(&h.EntityType, &h.EntityID, &h.OwnerID, &h.Title,
			&h.Snippet, &h.Score, &meta, &h.UpdatedAt); err != nil {
			return nil, fmt.Errorf("search: scan hit: %w", err)
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &h.Metadata)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
