package util

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeEndpoint(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"plain url", "http://x/v1", "http://x/v1"},
		{"trailing slash", "http://x/v1/", "http://x/v1"},
		{"trailing slashes", "http://x/v1///", "http://x/v1"},
		{"leading and trailing whitespace", "  http://x/v1/  ", "http://x/v1"},
		{"only slash", "/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeEndpoint(tc.in))
		})
	}
}

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		name, specific, fallback, want string
	}{
		{"specific wins", "a", "b", "a"},
		{"empty specific yields fallback", "", "b", "b"},
		{"whitespace specific yields fallback", "   ", "b", "b"},
		{"specific is trimmed", "  a  ", "b", "a"},
		{"fallback not trimmed", "", "  b  ", "  b  "},
		{"both empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, FirstNonEmpty(tc.specific, tc.fallback))
		})
	}
}

func TestConfigFilePath(t *testing.T) {
	got, err := ConfigFilePath("foo.json")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(got, filepath.Join("diesel", "foo.json")),
		"path %q should end with diesel/foo.json", got)
	assert.True(t, filepath.IsAbs(got), "config path should be absolute")
}

func TestAtomicWriteFile_CreatesFileAndParents(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "deeper", "out.txt")

	err := AtomicWriteFile(target, []byte("hello"), 0o600)
	require.NoError(t, err)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// No leftover tempfile in the parent directory.
	entries, err := os.ReadDir(filepath.Dir(target))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasSuffix(e.Name(), ".tmp"),
			"leftover tempfile after atomic write: %s", e.Name())
	}
}

func TestAtomicWriteFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o600))

	require.NoError(t, AtomicWriteFile(target, []byte("new"), 0o600))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}

func TestHttpStatusError(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		snippet   int64
		wantMatch string
	}{
		{
			name:      "with body",
			status:    500,
			body:      "internal error",
			snippet:   512,
			wantMatch: "HTTP 500: internal error",
		},
		{
			name:      "empty body omits suffix",
			status:    404,
			body:      "",
			snippet:   512,
			wantMatch: "HTTP 404",
		},
		{
			name:      "whitespace body is trimmed away",
			status:    502,
			body:      "  \n  \t ",
			snippet:   512,
			wantMatch: "HTTP 502",
		},
		{
			name:      "body is truncated to snippet bytes",
			status:    400,
			body:      strings.Repeat("x", 10000),
			snippet:   16,
			wantMatch: "HTTP 400: " + strings.Repeat("x", 16),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL)
			require.NoError(t, err)
			t.Cleanup(func() { resp.Body.Close() })

			err = HTTPStatusError(resp, tc.snippet)
			require.Error(t, err)
			assert.Equal(t, tc.wantMatch, err.Error())
		})
	}
}
