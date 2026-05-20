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
