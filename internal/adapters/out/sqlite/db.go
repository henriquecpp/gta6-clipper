// Package sqlite implements the ClipRepository on SQLite (modernc.org/sqlite,
// pure Go) with sqlc-generated queries and goose migrations embedded in the binary.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// pragmas applied to every connection (CLAUDE.md §7).
const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"

// DSN builds the connection string for the database file at path.
func DSN(path string) string {
	return "file:" + filepath.ToSlash(path) + "?" + pragmas
}

// Open opens (creating if needed) the database at path, limits the pool to a
// single writer connection and applies pending migrations.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite allows a single writer; one connection avoids SQLITE_BUSY between
	// our own goroutines (a separate read pool only if a need is measured).
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close() // the ping error is the one worth reporting
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	if err := Migrate(ctx, db); err != nil {
		_ = db.Close() // the migration error is the one worth reporting
		return nil, err
	}
	return db, nil
}

// Migrate applies all pending goose migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sub)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// Backup writes a consistent copy of the database to dest using VACUUM INTO.
func Backup(ctx context.Context, db *sql.DB, dest string) error {
	if strings.TrimSpace(dest) == "" {
		return fmt.Errorf("backup destination is empty")
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	return nil
}
