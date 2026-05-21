package sms

import (
	"testing"

	"diesel/internal/settings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigFor_ClampsPollSeconds — a typo of 0 or 1 in the dialog
// shouldn't translate into hammering Twilio. configFor enforces the
// floor before the manager goroutine ever sees the value.
func TestConfigFor_ClampsPollSeconds(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, defaultPollSeconds},
		{-5, defaultPollSeconds},
		{1, minPollSeconds},
		{2, minPollSeconds},
		{minPollSeconds, minPollSeconds},
		{30, 30},
	}
	for _, tc := range cases {
		cfg := configFor(settings.AppSettings{EnableSMS: true, SMSPollSeconds: tc.in})
		assert.Equal(t, tc.want, cfg.pollSecs, "input %d", tc.in)
	}
}

// TestConfigFor_DropsBlankAllowed — the dialog's textarea always ends
// with a trailing newline. The split shouldn't leak an empty allowed
// entry that would silently authorize an empty From field.
func TestConfigFor_DropsBlankAllowed(t *testing.T) {
	cfg := configFor(settings.AppSettings{
		EnableSMS:         true,
		SMSAllowedNumbers: []string{"+15551111111", "  ", "", "+15552222222"},
	})
	assert.Equal(t, []string{"+15551111111", "+15552222222"}, cfg.allowed)
}

// TestConfig_Validate — fields are checked in order; the message names
// the missing one so the user can act on it.
func TestConfig_Validate(t *testing.T) {
	full := config{
		enabled: true,
		sid:     "ACtest",
		token:   "secret",
		from:    "+15550000000",
		allowed: []string{"+15551111111"},
	}
	require.NoError(t, full.validate())

	c := full
	c.sid = ""
	assert.ErrorContains(t, c.validate(), "account SID")

	c = full
	c.token = ""
	assert.ErrorContains(t, c.validate(), "auth token")

	c = full
	c.from = ""
	assert.ErrorContains(t, c.validate(), "from number")

	c = full
	c.allowed = nil
	assert.ErrorContains(t, c.validate(), "allowed")
}

// TestIsAllowed_NormalizesFormatting — a user typing the number with
// human-friendly formatting in the dialog should still authorize
// inbound messages where Twilio always reports plain E.164.
func TestIsAllowed_NormalizesFormatting(t *testing.T) {
	cfg := config{allowed: []string{"+1 (555) 111-1111", "+15552222222"}}
	assert.True(t, cfg.isAllowed("+15551111111"))
	assert.True(t, cfg.isAllowed("+1-555-222-2222"))
	assert.False(t, cfg.isAllowed("+15553333333"))
}

// TestSeenSet_FIFO — the dedup buffer evicts oldest-first once cap is
// hit. The cap is just a memory bound; correctness only requires that
// recent IDs stick around long enough to cover poll overlap.
func TestSeenSet_FIFO(t *testing.T) {
	s := newSeenSet(3)
	s.add("a")
	s.add("b")
	s.add("c")
	assert.True(t, s.has("a"))
	s.add("d")
	assert.False(t, s.has("a"), "oldest should evict when cap is reached")
	assert.True(t, s.has("b"))
	assert.True(t, s.has("c"))
	assert.True(t, s.has("d"))
}

// TestSeenSet_AddIdempotent — re-adding a known SID shouldn't shuffle
// the eviction order. The poller adds before it knows whether a turn
// will succeed, and a retry that comes back with the same SID
// shouldn't promote it past newer messages.
func TestSeenSet_AddIdempotent(t *testing.T) {
	s := newSeenSet(2)
	s.add("a")
	s.add("b")
	s.add("a") // no-op
	s.add("c") // should evict "a"
	assert.False(t, s.has("a"))
	assert.True(t, s.has("b"))
	assert.True(t, s.has("c"))
}

// TestSeenSet_SnapshotRoundtrip — the snapshot preserves insertion
// order so reloading on restart restores the same FIFO eviction state.
// This is the contract savePollState relies on.
func TestSeenSet_SnapshotRoundtrip(t *testing.T) {
	src := newSeenSet(3)
	src.add("a")
	src.add("b")
	src.add("c")
	snap := src.snapshot()
	assert.Equal(t, []string{"a", "b", "c"}, snap)

	dst := newSeenSet(3)
	for _, id := range snap {
		dst.add(id)
	}
	dst.add("d") // should evict "a" — the oldest in the restored order
	assert.False(t, dst.has("a"))
	assert.True(t, dst.has("b"))
	assert.True(t, dst.has("c"))
	assert.True(t, dst.has("d"))
}

// TestConfig_Equal — used by Apply to skip a goroutine bounce on a
// no-op save. Order in the allowed list is part of the identity.
func TestConfig_Equal(t *testing.T) {
	a := config{
		enabled: true, sid: "AC", token: "tk", from: "+1", pollSecs: 10,
		allowed: []string{"+15551111111", "+15552222222"},
	}
	b := a
	assert.True(t, a.equal(b))

	b.allowed = []string{"+15552222222", "+15551111111"} // order matters
	assert.False(t, a.equal(b))

	b = a
	b.pollSecs = 11
	assert.False(t, a.equal(b))
}
