// Package storage is Diesel's single SQLite-backed persistence layer.
//
// One *Store, opened at startup and closed at shutdown, owns the
// conversation log, the settings blob, and the per-bridge bookkeeping
// that used to live in scattered JSON files. Conversation and settings
// have typed accessors; the small, bridge-specific state blobs go through
// the generic key/value accessors so the storage package never has to
// import the bridge packages.
//
// Concurrency: WAL mode lets readers run without blocking the writer, and
// busy_timeout lets the handful of writer goroutines (hub, SMS poll/
// dispatch, Telegram poll/dispatch) serialize at the SQLite layer instead
// of failing with SQLITE_BUSY.
package storage

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the SQLite connection pool. All Diesel persistence flows
// through one Store.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) the SQLite database at path, applies
// any pending migrations, and returns a ready Store.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("storage: create dir: %w", err)
	}
	// modernc takes pragmas as repeated _pragma query params.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}
	// The database holds plaintext API keys and tokens — match the 0600
	// posture the old settings.json had. The -wal/-shm sidecars inherit
	// this mode from the main file when SQLite creates them.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: chmod: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate applies the embedded goose migrations. The dialect is "sqlite3"
// (goose's SQL flavor) regardless of the modernc driver name.
func migrate(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("storage: goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("storage: migrate: %w", err)
	}
	return nil
}

// Close closes the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }
