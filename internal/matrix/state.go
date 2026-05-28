package matrix

import (
	"context"
	"crypto/rand"
	"encoding/json"

	"diesel/internal/storage"

	"maunium.net/go/mautrix/id"
)

// kv keys used by the bridge. Each row is one JSON blob, stored apart
// from the settings record so the sync loop and the dispatch loop never
// contend on a single row. mautrix-go manages its own tables (crypto_*,
// mx_*) alongside these via dbutil — separate schema, same database.
const (
	// pickleKeyKey holds the 32 random bytes mautrix-go uses to encrypt
	// Olm sessions at rest. Generated once on first enable; persisted
	// because a different key would render the existing crypto store
	// undecryptable.
	pickleKeyKey = "matrix_pickle_key"
	// homeserverKey caches the .well-known-discovered homeserver URL
	// so subsequent restarts don't re-fetch the discovery JSON.
	homeserverKey = "matrix_homeserver"
	// portraitsKey holds the per-room event ID of the last portrait we
	// posted, so the next portrait can redact the previous one — the
	// Matrix analogue of telegram_portraits.
	portraitsKey = "matrix_portraits"
)

// pickleKeyBytes is the mautrix-recommended pickle-key length. Fixed so
// a partial write doesn't yield a usable-but-shorter key.
const pickleKeyBytes = 32

// loadPickleKey reads the persisted pickle key. The bool is false on a
// fresh install or a corrupt/short record — the caller generates a new
// one in that case via generatePickleKey + savePickleKey.
func loadPickleKey(ctx context.Context, store *storage.Store) ([]byte, bool) {
	raw, ok, err := store.KVGet(ctx, pickleKeyKey)
	if err != nil || !ok || len(raw) < pickleKeyBytes {
		return nil, false
	}
	key := make([]byte, pickleKeyBytes)
	copy(key, raw)
	return key, true
}

// savePickleKey persists the pickle key. Errors are surfaced so the
// caller can log them; failure here means the next restart will mint a
// new key and lose access to existing Olm sessions.
func savePickleKey(ctx context.Context, store *storage.Store, key []byte) error {
	return store.KVSet(ctx, pickleKeyKey, key)
}

// generatePickleKey returns 32 fresh random bytes — used on the very
// first enable, before any Olm sessions exist. crypto/rand cannot return
// short reads on a healthy system; an error here is fatal enough that
// the caller surfaces it as a status-row error and refuses to start.
func generatePickleKey() ([]byte, error) {
	key := make([]byte, pickleKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// loadHomeserverURL reads the cached homeserver base URL. Empty when no
// record exists (first enable) — the caller falls back to .well-known
// discovery and persists the result.
func loadHomeserverURL(ctx context.Context, store *storage.Store) string {
	raw, ok, err := store.KVGet(ctx, homeserverKey)
	if err != nil || !ok {
		return ""
	}
	return string(raw)
}

// saveHomeserverURL persists the discovered homeserver URL.
func saveHomeserverURL(ctx context.Context, store *storage.Store, url string) error {
	return store.KVSet(ctx, homeserverKey, []byte(url))
}

// loadPortraitState reads the per-room → portrait-event-ID map. A
// missing or corrupt record yields an empty (non-nil) map — same
// contract as telegram.loadPortraitState. JSON encodes both keys and
// values as strings, so the round-trip is lossless.
func loadPortraitState(ctx context.Context, store *storage.Store) map[id.RoomID]id.EventID {
	raw, ok, err := store.KVGet(ctx, portraitsKey)
	if err != nil || !ok {
		return map[id.RoomID]id.EventID{}
	}
	var m map[id.RoomID]id.EventID
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return map[id.RoomID]id.EventID{}
	}
	return m
}

// savePortraitState writes the per-room → portrait-event-ID map.
func savePortraitState(ctx context.Context, store *storage.Store, m map[id.RoomID]id.EventID) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return store.KVSet(ctx, portraitsKey, data)
}
