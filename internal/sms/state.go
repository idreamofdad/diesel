package sms

import (
	"context"
	"encoding/json"
	"time"

	"diesel/internal/storage"
)

// stateKey is the kv row holding the SMS poll bookkeeping. Stored apart
// from the settings blob because it's bookkeeping, not configuration —
// the user never edits it and it changes far more often.
const stateKey = "sms_state"

// pollState is the persisted record that lets the poller pick up where it
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

// loadPollState reads the persisted bookkeeping. A missing or
// unparseable record returns an empty struct — a fresh install just
// starts from "now" without a special-case branch in the caller.
func loadPollState(ctx context.Context, store *storage.Store) pollState {
	var s pollState
	raw, ok, err := store.KVGet(ctx, stateKey)
	if err != nil || !ok {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

// savePollState writes the bookkeeping. Errors are surfaced for the
// caller to log — a failed save means the next restart might re-process
// a message, which is annoying but not destructive.
func savePollState(ctx context.Context, store *storage.Store, s pollState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return store.KVSet(ctx, stateKey, data)
}
