package mcpauth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgMacroUserLookup resolves a macro user id from the provider email claim:
// User.id is "macro|<email>" (mirrors authentication::lookupMacroUser).
type PgMacroUserLookup struct {
	DB *pgxpool.Pool
}

// LookupMacroUserID implements MacroUserLookup.
func (l PgMacroUserLookup) LookupMacroUserID(ctx context.Context, providerUserID, email string) (string, error) {
	if email == "" {
		return "", fmt.Errorf("mcpauth: token has no email claim")
	}
	var id string
	err := l.DB.QueryRow(ctx,
		`SELECT id FROM "User" WHERE lower(email) = lower($1)`, email).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("mcpauth: no macro user for %s", email)
	}
	if err != nil {
		return "", fmt.Errorf("mcpauth: user lookup: %w", err)
	}
	return id, nil
}
