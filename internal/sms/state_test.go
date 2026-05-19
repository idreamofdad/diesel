package sms

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTempConfigDir points util.ConfigFilePath at a temp dir for the
// duration of the test. ConfigFilePath reads from XDG_CONFIG_HOME on
// Linux and from os.UserConfigDir's resolution elsewhere; setting HOME
// (and the XDG override on Linux) covers both.
func withTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	return dir
}

// TestPollState_Roundtrip — save → load should restore the cursor and
// the SID set exactly. This is the contract pollLoop relies on to
// avoid replaying messages across restarts.
func TestPollState_Roundtrip(t *testing.T) {
	withTempConfigDir(t)

	cursor := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	in := pollState{
		Cursor:   cursor,
		SeenSIDs: []string{"SM1", "SM2", "SM3"},
	}
	require.NoError(t, savePollState(in))

	out := loadPollState()
	assert.Equal(t, cursor, out.Cursor.UTC())
	assert.Equal(t, []string{"SM1", "SM2", "SM3"}, out.SeenSIDs)
}

// TestPollState_MissingFile — a fresh install with no state file
// returns a zero-valued struct (no error). The poll loop treats a zero
// cursor as "seed from now" so we don't pull historical messages.
func TestPollState_MissingFile(t *testing.T) {
	withTempConfigDir(t)
	out := loadPollState()
	assert.True(t, out.Cursor.IsZero())
	assert.Empty(t, out.SeenSIDs)
}

// TestPollState_CorruptFile — a half-written or hand-edited file
// shouldn't crash the loader. Same fall-through as loadPollState's
// docstring promises.
func TestPollState_CorruptFile(t *testing.T) {
	withTempConfigDir(t)
	path, err := statePath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	out := loadPollState()
	assert.True(t, out.Cursor.IsZero())
	assert.Empty(t, out.SeenSIDs)
}
