package staticfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MetadataObject mirrors the DynamoDB MetadataObject model (now a macrodb
// row — the self-host port replaces DynamoDB with Postgres).
type MetadataObject struct {
	FileID        string          `json:"file_id"`
	OwnerID       string          `json:"owner_id"`
	ContentType   string          `json:"content_type"`
	IsUploaded    bool            `json:"is_uploaded"`
	LastAccessed  time.Time       `json:"last_accessed"`
	ExtensionData json.RawMessage `json:"extension_data,omitempty"`
	FileName      string          `json:"file_name"`
	S3Key         string          `json:"s3_key"`
}

// ErrNotFound mirrors dynamodb::model::DeleteError::NotFound.
var ErrNotFound = errors.New("not found")

// MetadataStore is the repository port for static-file metadata.
type MetadataStore interface {
	PutMetadata(ctx context.Context, m MetadataObject) error
	GetMetadata(ctx context.Context, fileID string) (*MetadataObject, error)
	DeleteMetadata(ctx context.Context, fileID string) error
	MarkUploaded(ctx context.Context, fileID string) error
	BulkGetMetadata(ctx context.Context, fileIDs []string) (map[string]MetadataObject, error)
	BulkDeleteMetadata(ctx context.Context, fileIDs []string) []error
}

// PgMetadataStore is the Postgres (macrodb) implementation.
type PgMetadataStore struct {
	db *pgxpool.Pool
}

func NewPgMetadataStore(db *pgxpool.Pool) *PgMetadataStore {
	return &PgMetadataStore{db: db}
}

// EnsureSchema creates the metadata table when missing. Replaces the DynamoDB
// table that existed in the AWS deployment.
// TODO(migrate): move this into crates/macro_db_client/migrations.
func (s *PgMetadataStore) EnsureSchema(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS static_files (
			file_id        TEXT PRIMARY KEY,
			owner_id       TEXT NOT NULL,
			content_type   TEXT NOT NULL,
			is_uploaded    BOOLEAN NOT NULL DEFAULT false,
			last_accessed  TIMESTAMPTZ NOT NULL DEFAULT now(),
			extension_data JSONB,
			file_name      TEXT NOT NULL,
			s3_key         TEXT NOT NULL
		)`)
	return err
}

func (s *PgMetadataStore) PutMetadata(ctx context.Context, m MetadataObject) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO static_files
			(file_id, owner_id, content_type, is_uploaded, last_accessed, extension_data, file_name, s3_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (file_id) DO UPDATE SET
			owner_id = EXCLUDED.owner_id,
			content_type = EXCLUDED.content_type,
			is_uploaded = EXCLUDED.is_uploaded,
			last_accessed = EXCLUDED.last_accessed,
			extension_data = EXCLUDED.extension_data,
			file_name = EXCLUDED.file_name,
			s3_key = EXCLUDED.s3_key`,
		m.FileID, m.OwnerID, m.ContentType, m.IsUploaded, m.LastAccessed,
		m.ExtensionData, m.FileName, m.S3Key)
	return err
}

const metadataCols = `file_id, owner_id, content_type, is_uploaded, last_accessed, extension_data, file_name, s3_key`

func scanMetadata(row pgx.Row) (*MetadataObject, error) {
	var m MetadataObject
	var ext []byte
	err := row.Scan(&m.FileID, &m.OwnerID, &m.ContentType, &m.IsUploaded,
		&m.LastAccessed, &ext, &m.FileName, &m.S3Key)
	if err != nil {
		return nil, err
	}
	if ext != nil {
		m.ExtensionData = ext
	}
	return &m, nil
}

func (s *PgMetadataStore) GetMetadata(ctx context.Context, fileID string) (*MetadataObject, error) {
	m, err := scanMetadata(s.db.QueryRow(ctx,
		`SELECT `+metadataCols+` FROM static_files WHERE file_id = $1`, fileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get metadata %s: %w", fileID, err)
	}
	return m, nil
}

func (s *PgMetadataStore) DeleteMetadata(ctx context.Context, fileID string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM static_files WHERE file_id = $1`, fileID)
	if err != nil {
		return fmt.Errorf("delete metadata %s: %w", fileID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PgMetadataStore) MarkUploaded(ctx context.Context, fileID string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE static_files SET is_uploaded = true WHERE file_id = $1`, fileID)
	return err
}

func (s *PgMetadataStore) BulkGetMetadata(ctx context.Context, fileIDs []string) (map[string]MetadataObject, error) {
	out := make(map[string]MetadataObject, len(fileIDs))
	if len(fileIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT `+metadataCols+` FROM static_files WHERE file_id = ANY($1)`, fileIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MetadataObject
		var ext []byte
		if err := rows.Scan(&m.FileID, &m.OwnerID, &m.ContentType, &m.IsUploaded,
			&m.LastAccessed, &ext, &m.FileName, &m.S3Key); err != nil {
			return nil, err
		}
		if ext != nil {
			m.ExtensionData = ext
		}
		out[m.FileID] = m
	}
	return out, rows.Err()
}

func (s *PgMetadataStore) BulkDeleteMetadata(ctx context.Context, fileIDs []string) []error {
	results := make([]error, len(fileIDs))
	for i, id := range fileIDs {
		results[i] = s.DeleteMetadata(ctx, id)
	}
	return results
}
