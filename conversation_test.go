package main

import (
	"context"
	"os"
	"testing"

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

func TestSaveLoadConversation_RoundTrip(t *testing.T) {
	withTempConfigDir(t)

	original := []chatMessage{
		{Role: roleUser, Content: "hello"},
		{Role: roleAssistant, Content: "hi there"},
		{Role: roleUser, Content: "how are you?"},
	}
	require.NoError(t, saveConversation(context.Background(),original))

	got := loadConversation()
	assert.Equal(t, original, got)
}

func TestSaveConversation_EmptyHistoryRemovesFile(t *testing.T) {
	withTempConfigDir(t)

	// Seed a file, then save an empty history — the file should be
	// removed so the next launch starts clean.
	require.NoError(t, saveConversation(context.Background(),[]chatMessage{
		{Role: roleUser, Content: "x"},
	}))
	path, err := conversationPath()
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err, "file should exist after seeding")

	require.NoError(t, saveConversation(context.Background(),nil))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should be removed when saving empty history")
}

func TestSaveConversation_EmptyOnFreshDirIsNoop(t *testing.T) {
	withTempConfigDir(t)

	// No file yet, save empty — should not error out trying to remove
	// a file that doesn't exist.
	assert.NoError(t, saveConversation(context.Background(),nil))
}

func TestLoadConversation_MissingFileReturnsNil(t *testing.T) {
	withTempConfigDir(t)
	assert.Nil(t, loadConversation())
}

func TestLoadConversation_CorruptFileReturnsNil(t *testing.T) {
	withTempConfigDir(t)
	path, err := conversationPath()
	require.NoError(t, err)
	require.NoError(t, atomicWriteFile(path, []byte("not json"), 0o600))

	assert.Nil(t, loadConversation())
}

func TestSaveConversation_OverwritesExisting(t *testing.T) {
	withTempConfigDir(t)

	first := []chatMessage{{Role: roleUser, Content: "first"}}
	require.NoError(t, saveConversation(context.Background(),first))

	second := []chatMessage{
		{Role: roleUser, Content: "second"},
		{Role: roleAssistant, Content: "reply"},
	}
	require.NoError(t, saveConversation(context.Background(),second))

	got := loadConversation()
	assert.Equal(t, second, got, "second save should fully replace the first")
}
