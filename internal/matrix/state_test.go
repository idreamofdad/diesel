package matrix

import (
	"context"
	"path/filepath"
	"testing"

	"diesel/internal/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/id"
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

// TestPortraitState_Roundtrip — save → load restores the room →
// portrait-event-ID map. JSON encodes both keys and values as strings;
// the round-trip preserves them losslessly so a restart can still
// redact stale portraits.
func TestPortraitState_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	in := map[id.RoomID]id.EventID{
		"!abc:matrix.org": "$evt1:matrix.org",
		"!xyz:example":    "$evt2:example",
	}
	require.NoError(t, savePortraitState(ctx, store, in))

	assert.Equal(t, in, loadPortraitState(ctx, store))
}

// TestPortraitState_Missing — a fresh install has no record; load
// yields an empty, non-nil map the dispatch loop can index freely.
func TestPortraitState_Missing(t *testing.T) {
	out := loadPortraitState(context.Background(), newTestStore(t))
	assert.NotNil(t, out)
	assert.Empty(t, out)
}

// TestPortraitState_CorruptValue — a malformed stored value must not
// crash the loader; it falls back to an empty map.
func TestPortraitState_CorruptValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.KVSet(ctx, portraitsKey, []byte("{not json")))

	out := loadPortraitState(ctx, store)
	assert.NotNil(t, out)
	assert.Empty(t, out)
}
