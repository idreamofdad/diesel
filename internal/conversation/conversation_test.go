package conversation

import (
	"context"
	"os"
	"testing"

	"diesel/internal/chat"
	"diesel/internal/util"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTempConfigDir redirects os.UserConfigDir to a fresh tempdir for
// the duration of the test, so the persisted conversation lands in
// isolation from the user's real Diesel state.
func withTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	return dir
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	withTempConfigDir(t)

	original := []chat.Message{
		{Role: chat.RoleUser, Content: "hello"},
		{Role: chat.RoleAssistant, Content: "hi there"},
		{Role: chat.RoleUser, Content: "how are you?"},
	}
	require.NoError(t, Save(context.Background(), original))

	got := Load()
	assert.Equal(t, original, got)
}

func TestSave_EmptyHistoryRemovesFile(t *testing.T) {
	withTempConfigDir(t)

	// Seed a file, then save an empty history — the file should be
	// removed so the next launch starts clean.
	require.NoError(t, Save(context.Background(), []chat.Message{
		{Role: chat.RoleUser, Content: "x"},
	}))
	path, err := conversationPath()
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err, "file should exist after seeding")

	require.NoError(t, Save(context.Background(), nil))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should be removed when saving empty history")
}

func TestSave_EmptyOnFreshDirIsNoop(t *testing.T) {
	withTempConfigDir(t)

	// No file yet, save empty — should not error out trying to remove
	// a file that doesn't exist.
	assert.NoError(t, Save(context.Background(), nil))
}

func TestLoad_MissingFileReturnsNil(t *testing.T) {
	withTempConfigDir(t)
	assert.Nil(t, Load())
}

func TestLoad_CorruptFileReturnsNil(t *testing.T) {
	withTempConfigDir(t)
	path, err := conversationPath()
	require.NoError(t, err)
	require.NoError(t, util.AtomicWriteFile(path, []byte("not json"), 0o600))

	assert.Nil(t, Load())
}

func TestSave_OverwritesExisting(t *testing.T) {
	withTempConfigDir(t)

	first := []chat.Message{{Role: chat.RoleUser, Content: "first"}}
	require.NoError(t, Save(context.Background(), first))

	second := []chat.Message{
		{Role: chat.RoleUser, Content: "second"},
		{Role: chat.RoleAssistant, Content: "reply"},
	}
	require.NoError(t, Save(context.Background(), second))

	got := Load()
	assert.Equal(t, second, got, "second save should fully replace the first")
}
