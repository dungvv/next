package convert

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// bomPart mirrors model::document::BomPart (subset used by backfill).
type bomPart struct {
	SHA  string `json:"sha"`
	Path string `json:"path"`
	ID   string `json:"id"`
}

// docxDocument is the subset of DocumentMetadata the backfill needs.
type docxDocument struct {
	DocumentID string
	Owner      string // owner principal string (e.g. "macro|user@x.com")
	BomParts   []bomPart
}

// PgDocxQuerier implements docxQuerier against macrodb, porting
// macro_db_client::convert::get_docx_files.
type PgDocxQuerier struct {
	db *pgxpool.Pool
}

func NewPgDocxQuerier(db *pgxpool.Pool) *PgDocxQuerier {
	return &PgDocxQuerier{db: db}
}

func (q *PgDocxQuerier) DocxFiles(ctx context.Context, limit, offset int64) ([]docxDocument, int64, error) {
	var count int64
	if err := q.db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "Document" d
		WHERE d."fileType" = 'docx' AND d."deletedAt" IS NULL AND d.uploaded = true`).Scan(&count); err != nil {
		return nil, 0, fmt.Errorf("count docx files: %w", err)
	}
	if count == 0 {
		return nil, 0, nil
	}

	rows, err := q.db.Query(ctx, `
		SELECT
			d.id AS document_id,
			d.owner AS owner,
			db.bom_parts AS document_bom
		FROM "Document" d
		LEFT JOIN LATERAL (
			SELECT
				b.id,
				(
					SELECT json_agg(
						json_build_object(
							'id', bp.id,
							'sha', bp.sha,
							'path', bp.path
						)
					)
					FROM "BomPart" bp
					WHERE bp."documentBomId" = b.id
				) AS bom_parts
			FROM "DocumentBom" b
			WHERE b."documentId" = d.id
			ORDER BY b."createdAt" DESC
			LIMIT 1
		) db ON true
		WHERE d."fileType" = 'docx' AND d."deletedAt" IS NULL AND d.uploaded = true
		ORDER BY d."updatedAt" DESC
		LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("get docx files: %w", err)
	}
	defer rows.Close()

	var docs []docxDocument
	for rows.Next() {
		var d docxDocument
		var bom []byte
		if err := rows.Scan(&d.DocumentID, &d.Owner, &bom); err != nil {
			return nil, 0, err
		}
		if bom != nil {
			if err := json.Unmarshal(bom, &d.BomParts); err != nil {
				return nil, 0, fmt.Errorf("decode bom parts for %s: %w", d.DocumentID, err)
			}
		}
		docs = append(docs, d)
	}
	return docs, count, rows.Err()
}
