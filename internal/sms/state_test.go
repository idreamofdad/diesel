package sms

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

// TestPollState_Roundtrip — save → load should restore the cursor and
// the SID set exactly. This is the contract pollLoop relies on to avoid
// replaying messages across restarts.
func TestPollState_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cursor := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	in := pollState{Cursor: cursor, SeenSIDs: []string{"SM1", "SM2", "SM3"}}
	require.NoError(t, savePollState(ctx, store, in))

	out := loadPollState(ctx, store)
	assert.Equal(t, cursor, out.Cursor.UTC())
	assert.Equal(t, []string{"SM1", "SM2", "SM3"}, out.SeenSIDs)
}

// TestPollState_Missing — a fresh install with no stored record returns a
// zero-valued struct. The poll loop treats a zero cursor as "seed from
// now" so we don't pull historical messages.
func TestPollState_Missing(t *testing.T) {
	out := loadPollState(context.Background(), newTestStore(t))
	assert.True(t, out.Cursor.IsZero())
	assert.Empty(t, out.SeenSIDs)
}

// TestPollState_CorruptValue — a malformed stored value shouldn't crash
// the loader; it falls through to a zero struct, same as a missing one.
func TestPollState_CorruptValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.KVSet(ctx, stateKey, []byte("{not json")))

	out := loadPollState(ctx, store)
	assert.True(t, out.Cursor.IsZero())
	assert.Empty(t, out.SeenSIDs)
}
