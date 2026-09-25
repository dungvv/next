// Package migrate applies the shared sqlx-format migrations
// (crates/macro_db_client/migrations, "<version>_<desc>.sql") to each of the
// databases, writing sqlx-compatible `_sqlx_migrations` rows so a Go
// deployment can coexist with the Rust stack on the same data.
package migrate

import (
	"context"
	"crypto/sha512"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/macro-inc/macro/pkg/config"
)

type migration struct {
	version       int64
	desc          string
	path          string
	noTransaction bool
}

// Run applies pending migrations to all configured databases.
func Run(ctx context.Context, cfg config.Config, dir string) error {
	migrations, err := load(dir)
	if err != nil {
		return err
	}
	for name, url := range map[string]string{
		"macrodb": cfg.MacroDBURL, "email": cfg.EmailDBURL,
		"comms": cfg.CommsDBURL, "notification": cfg.NotificationDBURL,
	} {
		if err := applyAll(ctx, url, migrations); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Printf("%s: up to date (%d migrations)\n", name, len(migrations))
	}
	return nil
}

func load(dir string) ([]migration, error) {
	var out []migration
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".sql") {
			return err
		}
		// sqlx reversible migrations ship .down.sql companions that must never
		// be applied forward.
		if strings.HasSuffix(d.Name(), ".down.sql") {
			return nil
		}
		base := strings.TrimSuffix(d.Name(), ".sql")
		base = strings.TrimSuffix(base, ".up")
		v, desc, ok := strings.Cut(base, "_")
		if !ok {
			return fmt.Errorf("bad migration filename: %s", d.Name())
		}
		version, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("bad migration version in %s: %w", d.Name(), err)
		}
		head, err := readHead(path)
		if err != nil {
			return err
		}
		out = append(out, migration{
			version:       version,
			desc:          strings.ReplaceAll(desc, "_", " "),
			path:          path,
			noTransaction: strings.Contains(head, "-- no-transaction"),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// readHead returns the first lines of a file, where sqlx directives live.
func readHead(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return string(buf[:n]), nil
}

// sqlx advisory lock key (arbitrary but fixed).
const lockKey int64 = 0x6d6163726f2d6d69 // "macro-mi"

func applyAll(ctx context.Context, url string, migrations []migration) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// Session-scoped advisory lock so concurrent migrators (Go + Rust,
	// rolling deploys) serialize like sqlx does.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS _sqlx_migrations (
		version BIGINT PRIMARY KEY,
		description TEXT NOT NULL,
		installed_on TIMESTAMPTZ NOT NULL DEFAULT now(),
		success BOOLEAN NOT NULL,
		checksum BYTEA NOT NULL,
		execution_time BIGINT NOT NULL)`); err != nil {
		return err
	}

	applied := map[int64][]byte{}
	var failed int64
	rows, err := conn.Query(ctx, `SELECT version, checksum, success FROM _sqlx_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int64
		var checksum []byte
		var ok bool
		if err := rows.Scan(&v, &checksum, &ok); err != nil {
			rows.Close()
			return err
		}
		if !ok {
			failed = v
		}
		applied[v] = checksum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if failed != 0 {
		return fmt.Errorf("migration %d previously failed; fix the DB before continuing", failed)
	}
	for v := range applied {
		if !containsVersion(migrations, v) {
			return fmt.Errorf("database has applied migration %d missing from source; refusing to run on a divergent schema", v)
		}
	}

	for _, m := range migrations {
		sql, err := os.ReadFile(m.path)
		if err != nil {
			return err
		}
		sum := sha512.Sum384(sql)
		if prev, ok := applied[m.version]; ok {
			if string(prev) != string(sum[:]) {
				return fmt.Errorf("migration %d checksum mismatch (was applied by sqlx?)", m.version)
			}
			continue
		}
		if err := applyOne(ctx, conn, m, sql, sum[:]); err != nil {
			return err
		}
		fmt.Printf("  applied %d %s\n", m.version, m.desc)
	}
	return nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, m migration, sql, checksum []byte) error {
	start := time.Now()

	if m.noTransaction {
		// sqlx runs these without a wrapping transaction (e.g. CREATE INDEX
		// CONCURRENTLY). Apply in autocommit, then record in its own tx.
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.desc, err)
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO _sqlx_migrations (version, description, success, checksum, execution_time)
			 VALUES ($1, $2, true, $3, $4)`,
			m.version, m.desc, checksum, time.Since(start).Nanoseconds()); err != nil {
			return err
		}
		return nil
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("migration %d (%s): %w", m.version, m.desc, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO _sqlx_migrations (version, description, success, checksum, execution_time)
		 VALUES ($1, $2, true, $3, $4)`,
		m.version, m.desc, checksum, time.Since(start).Nanoseconds()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func containsVersion(migrations []migration, v int64) bool {
	for _, m := range migrations {
		if m.version == v {
			return true
		}
	}
	return false
}
