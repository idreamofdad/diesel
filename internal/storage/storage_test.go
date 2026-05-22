package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"diesel/internal/chat"
	"diesel/internal/settings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTestStore opens a real SQLite database in a temp dir — exercising
// migrations, WAL, and file permissions on the actual code path. The
// directory (and the -wal/-shm sidecars) are cleaned up with the test.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "diesel.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestOpen_AppliesMigrations — a freshly opened database has both tables.
func TestOpen_AppliesMigrations(t *testing.T) {
	st := openTestStore(t)
	_, err := st.db.Exec(`INSERT INTO kv (key, value) VALUES ('x', 'y')`)
	require.NoError(t, err)
	var n int
	require.NoError(t, st.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n))
	assert.Equal(t, 0, n)
}

// TestOpen_Idempotent — reopening an existing database re-runs the
// migration step harmlessly.
func TestOpen_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diesel.db")
	st1, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, st1.Close())

	st2, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, st2.Close())
}

// TestConversation_RoundTrip — appended messages load back with every
// field intact, including the emotion/naked bookkeeping.
func TestConversation_RoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	got, err := st.LoadConversation(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)

	user := chat.Message{
		Role:      chat.RoleUser,
		Content:   "hi",
		Timestamp: time.Now().UTC().Round(time.Millisecond),
	}
	asst := chat.Message{
		Role:      chat.RoleAssistant,
		Content:   "hey",
		Emotion:   "happy",
		Naked:     true,
		Timestamp: time.Now().UTC().Round(time.Millisecond),
	}
	require.NoError(t, st.AppendMessages(ctx, user, asst))

	got, err = st.LoadConversation(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, chat.RoleUser, got[0].Role)
	assert.Equal(t, "hi", got[0].Content)
	assert.True(t, got[0].Timestamp.Equal(user.Timestamp))
	assert.Equal(t, "happy", got[1].Emotion)
	assert.True(t, got[1].Naked)
}

// TestConversation_AppendPreservesOrder — separate appends keep insertion
// order on load.
func TestConversation_AppendPreservesOrder(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, st.AppendMessages(ctx, chat.Message{Role: chat.RoleUser, Content: "1"}))
	require.NoError(t, st.AppendMessages(ctx, chat.Message{Role: chat.RoleUser, Content: "2"}))

	got, err := st.LoadConversation(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "1", got[0].Content)
	assert.Equal(t, "2", got[1].Content)
}

// TestAppendMessages_Empty — appending nothing is a no-op, not an error.
func TestAppendMessages_Empty(t *testing.T) {
	require.NoError(t, openTestStore(t).AppendMessages(context.Background()))
}

// TestClearConversation — clear wipes every message.
func TestClearConversation(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, st.AppendMessages(ctx, chat.Message{Role: chat.RoleUser, Content: "hi"}))
	require.NoError(t, st.ClearConversation(ctx))

	got, err := st.LoadConversation(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestSettings_RoundTrip — an absent row yields defaults; a saved blob
// loads back field-for-field.
func TestSettings_RoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	got, err := st.LoadSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, settings.Default(), got)

	in := settings.Default()
	in.Theme = "Light"
	in.APIKey = "sk-secret"
	require.NoError(t, st.SaveSettings(ctx, in))

	got, err = st.LoadSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Light", got.Theme)
	assert.Equal(t, "sk-secret", got.APIKey)
}

// TestKV_RoundTripAndMissing — KVGet reports absence, KVSet upserts.
func TestKV_RoundTripAndMissing(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	_, ok, err := st.KVGet(ctx, "absent")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, st.KVSet(ctx, "k", []byte("v1")))
	v, ok, err := st.KVGet(ctx, "k")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []byte("v1"), v)

	require.NoError(t, st.KVSet(ctx, "k", []byte("v2")))
	v, _, err = st.KVGet(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, []byte("v2"), v)
}
