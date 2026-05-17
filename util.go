package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	qt "github.com/mappu/miqt/qt6"
)

// normalizeEndpoint trims whitespace and any trailing slash so callers can
// append a fixed path without worrying about double slashes. Empty strings
// pass through unchanged.
func normalizeEndpoint(ep string) string {
	return strings.TrimRight(strings.TrimSpace(ep), "/")
}

// firstNonEmpty returns specific (trimmed) when it has content, otherwise
// the fallback unchanged. Used wherever STT/TTS-specific endpoints/keys
// fall back to the LLM-wide ones — the fallback is returned untrimmed
// because downstream normalization handles it.
func firstNonEmpty(specific, fallback string) string {
	if s := strings.TrimSpace(specific); s != "" {
		return s
	}
	return fallback
}

// configFilePath returns ~/.../diesel/<name> in the platform's user config
// directory. Used for settings.json, conversation.json, character.png, …
func configFilePath(name string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "diesel", name), nil
}

// atomicWriteFile writes `data` to `path` via a sibling tempfile + rename so
// a crash mid-write can't leave a half-finished file behind. Parent
// directories are created on demand.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// httpStatusError formats a non-2xx response as "HTTP <code>: <snippet>",
// reading up to `snippetBytes` of the body for the message. The shape every
// HTTP caller in this codebase produces, centralized so the call sites are
// one line.
func httpStatusError(resp *http.Response, snippetBytes int64) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, snippetBytes))
	body := strings.TrimSpace(string(snippet))
	if body == "" {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
}

// pollAsync runs `work` on a goroutine and delivers its result to `onDone`
// on the Qt main thread. A QTimer polls a buffered channel at `intervalMS`
// — the same shape used by the chat, TTS, and STT request paths so the UI
// loop keeps ticking while the HTTP call is in flight.
func pollAsync[T any](intervalMS int, work func() T, onDone func(T)) {
	ch := make(chan T, 1)
	go func() { ch <- work() }()
	poller := qt.NewQTimer()
	poller.SetSingleShot(false)
	poller.OnTimeout(func() {
		select {
		case r := <-ch:
			poller.Stop()
			onDone(r)
		default:
		}
	})
	poller.Start(intervalMS)
}