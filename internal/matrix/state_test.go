package matrix

import (
	"context"
	"path/filepath"
	"testing"

	"diesel/internal/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore opens a throwaway SQLite database in a temp dir, closed
// automatically when the test ends.
func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "diesel.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestPickleKey_Generate — fresh install: no record, so loadPickleKey
// reports false and the caller mints a new key. After save the next
// load returns the same bytes — the contract a restart relies on to
// keep Olm sessions decryptable.
func TestPickleKey_Generate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, found := loadPickleKey(ctx, store)
	assert.False(t, found)

	key, err := generatePickleKey()
	require.NoError(t, err)
	assert.Len(t, key, pickleKeyBytes)

	require.NoError(t, savePickleKey(ctx, store, key))
	got, found := loadPickleKey(ctx, store)
	assert.True(t, found)
	assert.Equal(t, key, got)
}

// TestPickleKey_Short — a truncated record (corrupt or partial write)
// must be treated as missing, not silently accepted. Returning a short
// key would render mautrix's crypto store undecryptable.
func TestPickleKey_Short(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.KVSet(ctx, pickleKeyKey, []byte("short")))

	_, found := loadPickleKey(ctx, store)
	assert.False(t, found)
}

// TestHomeserverURL_Roundtrip — save → load returns the cached URL.
// The cache is what lets a restart skip the .well-known discovery
// HTTP round-trip.
func TestHomeserverURL_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	assert.Equal(t, "", loadHomeserverURL(ctx, store))

	require.NoError(t, saveHomeserverURL(ctx, store, "https://matrix-client.matrix.org"))
	assert.Equal(t, "https://matrix-client.matrix.org", loadHomeserverURL(ctx, store))
}
