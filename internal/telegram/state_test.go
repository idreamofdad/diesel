package telegram

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTempConfigDir points util.ConfigFilePath at a temp dir for the
// duration of the test. ConfigFilePath reads from XDG_CONFIG_HOME on
// Linux and from os.UserConfigDir's resolution elsewhere; setting HOME
// (and the XDG override on Linux) covers both.
func withTempConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
}

// TestState_Roundtrip — save → load restores the offset and reports the
// file as found. This is the contract a restart relies on to resume the
// poll where it left off instead of re-skipping the backlog.
func TestState_Roundtrip(t *testing.T) {
	withTempConfigDir(t)

	require.NoError(t, saveState(state{Offset: 4242}))

	out, found := loadState()
	assert.True(t, found)
	assert.Equal(t, 4242, out.Offset)
}

// TestState_MissingFile — a fresh install has no state file. found is
// false, which the poll loop treats as "skip the backlog" rather than
// replaying up to 24 h of queued messages.
func TestState_MissingFile(t *testing.T) {
	withTempConfigDir(t)

	out, found := loadState()
	assert.False(t, found)
	assert.Zero(t, out.Offset)
}

// TestState_CorruptFile — a half-written or hand-edited file must not
// crash the loader. It falls into the same branch as a missing file:
// found is false, so the loop skips the backlog (safer than replaying).
func TestState_CorruptFile(t *testing.T) {
	withTempConfigDir(t)

	path, err := statePath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	out, found := loadState()
	assert.False(t, found)
	assert.Zero(t, out.Offset)
}
