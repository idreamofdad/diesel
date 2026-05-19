package sms

import (
	"encoding/json"
	"os"
	"time"

	"diesel/internal/util"
)

// stateFileName lives next to settings.json in the user config dir.
// Kept separate from settings because it's bookkeeping, not configuration —
// the user never edits it and it changes far more often than settings do,
// so persisting them together would force every save to rewrite the
// full settings blob.
const stateFileName = "sms_state.json"

// pollState is the on-disk record that lets the poller pick up where it
// left off across restarts. Without this, every restart processed the
// most-recent inbound message again because the in-memory dedup set
// reset and Twilio's DateSent filter only has day-level granularity.
//
// Cursor is the largest DateSent we've successfully processed; SeenSIDs
// is a bounded ring of recently-processed message SIDs that catches
// ties at the cursor's second (and protects against Twilio returning
// older rows the day filter can't exclude).
type pollState struct {
	Cursor   time.Time `json:"cursor"`
	SeenSIDs []string  `json:"seen_sids"`
}

func statePath() (string, error) {
	return util.ConfigFilePath(stateFileName)
}

// loadPollState reads the persisted bookkeeping. A missing or
// unparseable file returns an empty struct — same fall-through pattern
// as conversation.Load — so a fresh install just starts from "now"
// without a special-case branch in the caller.
func loadPollState() pollState {
	var s pollState
	path, err := statePath()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	return s
}

// savePollState writes the bookkeeping atomically. Errors are not
// surfaced — a failed save means the next restart might re-process a
// message, which is annoying but not destructive. The caller logs.
func savePollState(s pollState) error {
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
