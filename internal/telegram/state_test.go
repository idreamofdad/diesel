package telegram

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

// TestState_Roundtrip — save → load restores the offset and reports the
// record as found. This is the contract a restart relies on to resume
// the poll where it left off instead of re-skipping the backlog.
func TestState_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, saveState(ctx, store, state{Offset: 4242}))

	out, found := loadState(ctx, store)
	assert.True(t, found)
	assert.Equal(t, 4242, out.Offset)
}

// TestState_Missing — a fresh install has no stored record. found is
// false, which the poll loop treats as "skip the backlog" rather than
// replaying up to 24 h of queued messages.
func TestState_Missing(t *testing.T) {
	out, found := loadState(context.Background(), newTestStore(t))
	assert.False(t, found)
	assert.Zero(t, out.Offset)
}

// TestState_CorruptValue — a malformed stored value must not crash the
// loader. It falls into the same branch as a missing record: found is
// false, so the loop skips the backlog (safer than replaying).
func TestState_CorruptValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.KVSet(ctx, offsetKey, []byte("{not json")))

	out, found := loadState(ctx, store)
	assert.False(t, found)
	assert.Zero(t, out.Offset)
}

// TestPortraitState_Roundtrip — save → load restores the chat → portrait
// message map, including the negative chat IDs Telegram uses.
func TestPortraitState_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	in := map[int64]int{12345: 67, -100200300: 89}
	require.NoError(t, savePortraitState(ctx, store, in))

	assert.Equal(t, in, loadPortraitState(ctx, store))
}

// TestPortraitState_Missing — a fresh install has no record; load yields
// an empty, non-nil map the dispatch loop can index freely.
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
