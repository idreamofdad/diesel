package telegram

import (
	"context"
	"encoding/json"

	"diesel/internal/storage"
)

// offsetKey holds the getUpdates offset; portraitsKey holds the per-chat
// portrait-message-ID map. Stored apart from the settings blob because
// they're bookkeeping, not configuration, and change on every message.
// They remain separate kv rows so the poll loop and the dispatch loop
// never contend over a shared record.
const (
	offsetKey    = "telegram_offset"
	portraitsKey = "telegram_portraits"
)

// state is the persisted record that lets the poll loop resume across
// restarts. Offset is the next getUpdates offset — one past the highest
// update_id we've already accepted. The Telegram server also tracks an
// offset, but only advances it when we next call getUpdates with a
// higher value; this record is what lets a restart pick up messages that
// arrived while the app was down.
type state struct {
	Offset int `json:"offset"`
}

// loadState reads the persisted offset. The bool reports whether a
// usable record was found: false means a fresh install (or a wiped/
// corrupt record), which the poll loop treats as "skip the backlog"
// rather than replaying up to 24 h of queued messages. A corrupt record
// falling into the same branch is deliberate — skipping is safer than
// replaying.
func loadState(ctx context.Context, store *storage.Store) (state, bool) {
	var s state
	raw, ok, err := store.KVGet(ctx, offsetKey)
	if err != nil || !ok {
		return s, false
	}
	if json.Unmarshal(raw, &s) != nil {
		return s, false
	}
	return s, true
}

// saveState writes the offset. Errors are returned so the caller can log
// them; a failed save just means a restart might re-skip or re-handle
// around the boundary, which is annoying but not destructive.
func saveState(ctx context.Context, store *storage.Store, s state) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return store.KVSet(ctx, offsetKey, data)
}

// loadPortraitState reads the chat-ID → portrait-message-ID map. A
// missing or corrupt record yields an empty (non-nil) map — a fresh
// install simply has no prior portrait to clean up. JSON encodes the
// int64 keys as strings and decodes them back, so the round-trip is
// lossless.
func loadPortraitState(ctx context.Context, store *storage.Store) map[int64]int {
	raw, ok, err := store.KVGet(ctx, portraitsKey)
	if err != nil || !ok {
		return map[int64]int{}
	}
	var m map[int64]int
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return map[int64]int{}
	}
	return m
}

// savePortraitState writes the chat-ID → portrait-message-ID map. Errors
// are returned for the caller to log; a failed save just means a restart
// might miss one stale portrait.
func savePortraitState(ctx context.Context, store *storage.Store, m map[int64]int) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return store.KVSet(ctx, portraitsKey, data)
}
