package telegram

import (
	"strings"
	"testing"

	"diesel/internal/settings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigFor_NormalizesUsername — the dialog field is free-form: the
// user might type "@Alice", "alice", or pad it with spaces. configFor
// must strip the '@', lower-case, and trim before the manager sees it.
func TestConfigFor_NormalizesUsername(t *testing.T) {
	cfg := configFor(settings.AppSettings{
		EnableTelegram:          true,
		TelegramAllowedUsername: "  @Alice ",
	})
	assert.Equal(t, "alice", cfg.allowed)
}

// TestConfig_Validate — fields are checked in order; the message names
// the missing one so the Settings dialog status row is actionable.
func TestConfig_Validate(t *testing.T) {
	full := config{enabled: true, token: "123:ABC", allowed: "alice"}
	require.NoError(t, full.validate())

	c := full
	c.token = ""
	assert.ErrorContains(t, c.validate(), "bot token")

	c = full
	c.allowed = ""
	assert.ErrorContains(t, c.validate(), "allowed")
}

// TestIsAllowed_CaseInsensitive — Telegram usernames are case-insensitive
// and reported without the '@'. A sender typed into the dialog as
// "@Alice" must authorize the "alice" Telegram delivers, and nobody else.
func TestIsAllowed_CaseInsensitive(t *testing.T) {
	cfg := configFor(settings.AppSettings{TelegramAllowedUsername: "@Alice"})
	assert.True(t, cfg.isAllowed("alice"))
	assert.True(t, cfg.isAllowed("ALICE"))
	assert.True(t, cfg.isAllowed("@Alice"))
	assert.False(t, cfg.isAllowed("bob"))
}

// TestIsAllowed_EmptyUsername — a sender who never set a username can
// never match, so they're dropped. An unconfigured allow-list must not
// authorize a usernameless sender either: empty must not match empty.
func TestIsAllowed_EmptyUsername(t *testing.T) {
	cfg := config{allowed: "alice"}
	assert.False(t, cfg.isAllowed(""))
	assert.False(t, cfg.isAllowed("  "))

	empty := config{allowed: ""}
	assert.False(t, empty.isAllowed(""))
}

// TestConfig_Equal — used by Apply to skip a goroutine bounce on a no-op
// save. Order in the allowed list is part of the identity.
func TestConfig_Equal(t *testing.T) {
	a := config{enabled: true, token: "tk", allowed: "alice"}
	b := a
	assert.True(t, a.equal(b))

	b.allowed = "bob"
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
