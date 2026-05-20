package telegram

import (
	"strings"
	"testing"

	"diesel/internal/settings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigFor_NormalizesAllowed — the dialog's textarea is free-form:
// users type "@Alice", "alice", or a blank trailing line. configFor must
// strip the '@', lower-case, and drop blanks before the manager sees it.
func TestConfigFor_NormalizesAllowed(t *testing.T) {
	cfg := configFor(settings.AppSettings{
		EnableTelegram:           true,
		TelegramAllowedUsernames: []string{"@Alice", "  bob ", "", "  ", "@@weird"},
	})
	assert.Equal(t, []string{"alice", "bob", "@weird"}, cfg.allowed)
}

// TestConfig_Validate — fields are checked in order; the message names
// the missing one so the Settings dialog status row is actionable.
func TestConfig_Validate(t *testing.T) {
	full := config{enabled: true, token: "123:ABC", allowed: []string{"alice"}}
	require.NoError(t, full.validate())

	c := full
	c.token = ""
	assert.ErrorContains(t, c.validate(), "Bot Token")

	c = full
	c.allowed = nil
	assert.ErrorContains(t, c.validate(), "allowed")
}

// TestIsAllowed_CaseInsensitive — Telegram usernames are case-insensitive
// and reported without the '@'. A sender typed into the dialog as
// "@Alice" must authorize the "alice" Telegram delivers.
func TestIsAllowed_CaseInsensitive(t *testing.T) {
	cfg := configFor(settings.AppSettings{
		TelegramAllowedUsernames: []string{"@Alice", "bob"},
	})
	assert.True(t, cfg.isAllowed("alice"))
	assert.True(t, cfg.isAllowed("ALICE"))
	assert.True(t, cfg.isAllowed("@Bob"))
	assert.False(t, cfg.isAllowed("carol"))
}

// TestIsAllowed_EmptyUsername — a sender who never set a username can
// never match, so they're dropped. An empty allow-list entry must not
// turn into a wildcard that authorizes them.
func TestIsAllowed_EmptyUsername(t *testing.T) {
	cfg := config{allowed: []string{"alice"}}
	assert.False(t, cfg.isAllowed(""))
	assert.False(t, cfg.isAllowed("  "))

	empty := config{allowed: nil}
	assert.False(t, empty.isAllowed(""))
}

// TestConfig_Equal — used by Apply to skip a goroutine bounce on a no-op
// save. Order in the allowed list is part of the identity.
func TestConfig_Equal(t *testing.T) {
	a := config{enabled: true, token: "tk", allowed: []string{"alice", "bob"}}
	b := a
	assert.True(t, a.equal(b))

	b.allowed = []string{"bob", "alice"} // order matters
	assert.False(t, a.equal(b))

	b = a
	b.token = "other"
	assert.False(t, a.equal(b))

	b = a
	b.enabled = false
	assert.False(t, a.equal(b))
}

// TestIsStartCommand — /start is intercepted (canned greeting) whether
// it's bare, bot-addressed, or carries a deep-link payload. A message
// that merely starts with the letters "start" is a normal chat turn.
func TestIsStartCommand(t *testing.T) {
	for _, in := range []string{"/start", "/start@DieselBot", "/start ref123", "  /start  "} {
		assert.True(t, isStartCommand(in), in)
	}
	for _, in := range []string{"start", "hi there", "/started", "tell me about /start", ""} {
		assert.False(t, isStartCommand(in), in)
	}
}

// TestParseOrigin — the dispatch loop routes replies off the hub Origin.
// Only "telegram:<id>" origins are ours; desktop/web/sms turns must not
// be mistaken for Telegram ones.
func TestParseOrigin(t *testing.T) {
	id, ok := parseOrigin("telegram:123456789")
	assert.True(t, ok)
	assert.Equal(t, int64(123456789), id)

	id, ok = parseOrigin("telegram:-100200300") // group-style negative IDs
	assert.True(t, ok)
	assert.Equal(t, int64(-100200300), id)

	for _, bad := range []string{"desktop", "sms:+15551234567", "telegram:", "telegram:notanumber", ""} {
		_, ok := parseOrigin(bad)
		assert.False(t, ok, bad)
	}
}

// TestSplitMessage — replies are split on the Bot API's 4096-code-point
// ceiling. Short text passes through as a single chunk; an over-long
// echo is broken into pieces that each fit, with nothing lost.
func TestSplitMessage(t *testing.T) {
	assert.Equal(t, []string{"hello"}, splitMessage("hello"))

	exact := strings.Repeat("a", telegramMaxMessage)
	assert.Equal(t, []string{exact}, splitMessage(exact))

	long := strings.Repeat("b", telegramMaxMessage*2+50)
	chunks := splitMessage(long)
	assert.Len(t, chunks, 3)
	assert.Equal(t, long, strings.Join(chunks, ""))
	for _, c := range chunks {
		assert.LessOrEqual(t, len([]rune(c)), telegramMaxMessage)
	}
}
