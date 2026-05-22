package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"diesel/internal/settings"
)

// settingsKey is the kv row holding the JSON-encoded AppSettings blob.
const settingsKey = "settings"

// LoadSettings returns the persisted settings. A missing row yields
// defaults; a present row is decoded over a defaults-seeded struct so
// fields added since the blob was written keep their default value.
func (s *Store) LoadSettings(ctx context.Context) (settings.AppSettings, error) {
	raw, ok, err := s.KVGet(ctx, settingsKey)
	if err != nil {
		return settings.Default(), err
	}
	if !ok {
		return settings.Default(), nil
	}
	out := settings.Default()
	if err := json.Unmarshal(raw, &out); err != nil {
		return settings.Default(), fmt.Errorf("storage: decode settings: %w", err)
	}
	return out, nil
}

// SaveSettings persists the settings as a JSON blob.
func (s *Store) SaveSettings(ctx context.Context, st settings.AppSettings) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("storage: encode settings: %w", err)
	}
	return s.KVSet(ctx, settingsKey, data)
}
