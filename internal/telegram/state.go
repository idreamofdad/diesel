package telegram

import (
	"encoding/json"
	"os"

	"diesel/internal/util"
)

// stateFileName lives next to settings.json in the user config dir. Kept
// separate from settings because it's bookkeeping, not configuration —
// the user never edits it and it changes on every inbound message, so
// persisting it with settings would rewrite the whole settings blob each
// time.
const stateFileName = "telegram_state.json"

// state is the on-disk record that lets the poll loop resume across
// restarts. Offset is the next getUpdates offset — one past the highest
// update_id we've already accepted. The Telegram server also tracks an
// offset, but only advances it when we next call getUpdates with a
// higher value; this file is what lets a restart pick up messages that
// arrived while the app was down.
type state struct {
	Offset int `json:"offset"`
}

func statePath() (string, error) {
	return util.ConfigFilePath(stateFileName)
}

// loadState reads the persisted offset. The bool reports whether a
// usable file was found: false means a fresh install (or a wiped/corrupt
// file), which the poll loop treats as "skip the backlog" rather than
// replaying up to 24 h of queued messages. A corrupt file falling into
// the same branch is deliberate — skipping is safer than replaying.
func loadState() (state, bool) {
	var s state
	path, err := statePath()
	if err != nil {
		return s, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s, false
	}
	if json.Unmarshal(data, &s) != nil {
		return s, false
	}
	return s, true
}

// saveState writes the offset atomically. Errors are returned so the
// caller can log them; a failed save just means a restart might re-skip
// or re-handle around the boundary, which is annoying but not
// destructive.
func saveState(s state) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return util.AtomicWriteFile(path, data, 0o600)
}

// portraitStateFileName holds the per-chat record of which portrait
// photo is currently posted in each chat. It lets a restart still delete
// the previous portrait when a new one replaces it — without it the
// dispatch loop starts blank and orphans whatever was already posted.
//
// Kept separate from telegram_state.json on purpose: that file is owned
// and frequently rewritten by the poll loop, while this one is owned by
// the dispatch loop. Two files means neither goroutine can clobber the
// other's writes, so no lock is needed.
const portraitStateFileName = "telegram_portraits.json"

// loadPortraitState reads the chat-ID → portrait-message-ID map. A
// missing or corrupt file yields an empty (non-nil) map — a fresh
// install simply has no prior portrait to clean up. JSON encodes the
// int64 keys as strings and decodes them back, so the round-trip is
// lossless.
func loadPortraitState() map[int64]int {
	path, err := util.ConfigFilePath(portraitStateFileName)
	if err != nil {
		return map[int64]int{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[int64]int{}
	}
	var m map[int64]int
	if json.Unmarshal(data, &m) != nil || m == nil {
		return map[int64]int{}
	}
	return m
}

// savePortraitState writes the chat-ID → portrait-message-ID map
// atomically. Errors are returned for the caller to log; a failed save
// just means a restart might miss one stale portrait.
func savePortraitState(m map[int64]int) error {
	path, err := util.ConfigFilePath(portraitStateFileName)
	if err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return util.AtomicWriteFile(path, data, 0o600)
}
