// Package migrations applies AutoDeploy's forward-only PostgreSQL schema migrations.
package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
)

//go:embed *.sql
var files embed.FS
var migrationFiles fs.FS = files

// Apply records and applies each embedded migration exactly once.
func Apply(ctx context.Context, conn *pgx.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration run: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731492845631694122)`); err != nil {
		return fmt.Errorf("lock migration run: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT`); err != nil {
		return fmt.Errorf("add migration checksum: %w", err)
	}
	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && len(entry.Name()) > 4 && entry.Name()[len(entry.Name())-4:] == ".sql" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		contents, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(contents))
		var storedChecksum *string
		if err := tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE name = $1`, name).Scan(&storedChecksum); err != nil && err != pgx.ErrNoRows {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if storedChecksum != nil {
			if *storedChecksum != checksum {
				return fmt.Errorf("migration %s checksum mismatch", name)
			}
			continue
		}
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied {
			if _, err := tx.Exec(ctx, `UPDATE schema_migrations SET checksum=$2 WHERE name=$1 AND checksum IS NULL`, name, checksum); err != nil {
				return fmt.Errorf("backfill migration checksum %s: %w", name, err)
			}
			continue
		}
		if _, err = tx.Exec(ctx, string(contents)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)`, name, checksum)
		}
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration run: %w", err)
	}
	return nil
}
