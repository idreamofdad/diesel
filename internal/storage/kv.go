package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// KVGet returns the raw value stored under key. The bool is false when no
// row exists — callers treat that as a fresh start, exactly as a missing
// JSON file did before.
func (s *Store) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storage: kv get %q: %w", key, err)
	}
	return []byte(v), true, nil
}

// KVSet upserts value under key.
func (s *Store) KVSet(ctx context.Context, key string, value []byte) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, string(value),
	); err != nil {
		return fmt.Errorf("storage: kv set %q: %w", key, err)
	}
	return nil
}
