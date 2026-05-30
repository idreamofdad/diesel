package util

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// NormalizeEndpoint trims whitespace and any trailing slash so callers can
// append a fixed path without worrying about double slashes. Empty strings
// pass through unchanged.
func NormalizeEndpoint(ep string) string {
	return strings.TrimRight(strings.TrimSpace(ep), "/")
}

// FirstNonEmpty returns specific (trimmed) when it has content, otherwise
// the fallback unchanged. Used wherever STT/TTS-specific endpoints/keys
// fall back to the LLM-wide ones — the fallback is returned untrimmed
// because downstream normalization handles it.
func FirstNonEmpty(specific, fallback string) string {
	if s := strings.TrimSpace(specific); s != "" {
		return s
	}
	return fallback
}

// configDirOverride, when non-empty, replaces the default
// ~/.../diesel data directory. Set once at startup via SetConfigDir,
// before any ConfigFilePath call.
var configDirOverride string

// SetConfigDir overrides the directory ConfigFilePath places files in.
// An empty string restores the platform default. Intended to be called
// once at startup (e.g. from a -data-dir flag) before anything opens the
// database or reads a config file.
func SetConfigDir(dir string) {
	configDirOverride = dir
}

// ConfigFilePath returns the absolute path of <name> in Diesel's data
// directory — the SetConfigDir override when set, otherwise the
// platform's user config dir plus "diesel". Used for diesel.db,
// character.png, … The directory itself is created on demand by the
// callers that write into it (AtomicWriteFile, storage.Open).
func ConfigFilePath(name string) (string, error) {
	if configDirOverride != "" {
		return filepath.Join(configDirOverride, name), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "diesel", name), nil
}

// AtomicWriteFile writes `data` to `path` via a sibling tempfile + rename so
// a crash mid-write can't leave a half-finished file behind. Parent
// directories are created on demand.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// HTTPStatusError formats a non-2xx response as "HTTP <code>: <snippet>",
// reading up to `snippetBytes` of the body for the message. The shape every
// HTTP caller in this codebase produces, centralized so the call sites are
// one line.
func HTTPStatusError(resp *http.Response, snippetBytes int64) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, snippetBytes))
	body := strings.TrimSpace(string(snippet))
	if body == "" {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
}

// Async runs `work` on a goroutine and hands its result to `onDone`,
// exactly once. This is the shape the chat, TTS, and STT request paths use
// to keep a blocking HTTP call off the caller's thread while still getting
// the result back in one place.
//
// `onDone` runs on the worker goroutine — NOT on any GUI main thread — so a
// caller that touches widgets must marshal that work onto its toolkit's UI
// thread itself (e.g. fyne.Do). Keeping this package free of any GUI
// dependency is deliberate: util is imported across the whole tree, so it
// must not pull a windowing toolkit in behind it.
func Async[T any](work func() T, onDone func(T)) {
	go func() { onDone(work()) }()
}
