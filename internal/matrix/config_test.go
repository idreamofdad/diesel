package matrix

import (
	"testing"

	"diesel/internal/settings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/id"
)

// TestConfigFor_NormalizesMXID — the dialog field is free-form: the
// user might type "@Alice:Server.org" or "alice:server.org". configFor
// must lower-case the localpart and ensure a leading '@' before the
// manager sees it, so config equality, validation, and isAllowed all
// operate on canonical strings.
func TestConfigFor_NormalizesMXID(t *testing.T) {
	cfg := configFor(settings.AppSettings{
		EnableMatrix:      true,
		MatrixBotUserID:   "  @Diesel:Server.Org  ",
		MatrixAllowedUser: "alice:matrix.org",
		MatrixPassword:    "hunter2",
	})
	assert.Equal(t, "@diesel:Server.Org", cfg.botMXID)
	assert.Equal(t, "@alice:matrix.org", cfg.allowed)
}

// TestConfig_Validate — fields are checked in order; each message
// names the missing/invalid one so the Settings dialog status row is
// actionable.
func TestConfig_Validate(t *testing.T) {
	full := config{
		enabled:  true,
		botMXID:  "@diesel:matrix.org",
		password: "hunter2",
		allowed:  "@alice:matrix.org",
	}
	require.NoError(t, full.validate())

	c := full
	c.botMXID = ""
	assert.ErrorContains(t, c.validate(), "bot user ID")

	c = full
	c.botMXID = "diesel"
	assert.ErrorContains(t, c.validate(), "@name:server")

	c = full
	c.password = ""
	assert.ErrorContains(t, c.validate(), "password")

	c = full
	c.allowed = ""
	assert.ErrorContains(t, c.validate(), "allowed user")

	c = full
	c.allowed = "@diesel:matrix.org"
	assert.ErrorContains(t, c.validate(), "must differ")
}

// TestIsAllowed_CaseInsensitive — Matrix localparts are
// case-insensitive in practice and Element treats server names the
// same way. A sender typed into the dialog as "@Alice:Matrix.Org"
// must authorize the "@alice:matrix.org" the homeserver delivers,
// and nobody else.
func TestIsAllowed_CaseInsensitive(t *testing.T) {
	cfg := configFor(settings.AppSettings{MatrixAllowedUser: "@Alice:Matrix.Org"})
	assert.True(t, cfg.isAllowed("@alice:matrix.org"))
	assert.True(t, cfg.isAllowed("@ALICE:MATRIX.ORG"))
	assert.False(t, cfg.isAllowed("@bob:matrix.org"))
	assert.False(t, cfg.isAllowed(""))
}

// TestIsAllowed_EmptyAllowed — an unconfigured allow list must not
// authorize anyone, including a usernameless sender.
func TestIsAllowed_EmptyAllowed(t *testing.T) {
	empty := config{allowed: ""}
	assert.False(t, empty.isAllowed(""))
	assert.False(t, empty.isAllowed("@alice:matrix.org"))
}

// TestConfig_Equal — used by Apply to skip a goroutine bounce on a
// no-op save. Any field changing flips equality so the manager
// re-applies.
func TestConfig_Equal(t *testing.T) {
	a := config{enabled: true, botMXID: "@d:s", password: "p", allowed: "@a:s"}
	b := a
	assert.True(t, a.equal(b))

	b.allowed = "@b:s"
	assert.False(t, a.equal(b))

	b = a
	b.password = "other"
	assert.False(t, a.equal(b))

	b = a
	b.enabled = false
	assert.False(t, a.equal(b))
}

// TestIsValidMXID — the lightweight pre-flight check that gates Apply
// before we try to discover the homeserver. Anything missing the '@',
// a colon, or content on either side is rejected.
func TestIsValidMXID(t *testing.T) {
	good := []string{"@a:b", "@diesel:matrix.org", "@x:y.z"}
	for _, s := range good {
		assert.True(t, isValidMXID(s), s)
	}
	bad := []string{"", "@", "@:", ":server", "alice:server", "@alice", "@:server", "@alice:"}
	for _, s := range bad {
		assert.False(t, isValidMXID(s), s)
	}
}

// TestServerPart — used both for .well-known discovery and the
// TestConnection probe. Returns the part after the colon; empty on a
// malformed input.
func TestServerPart(t *testing.T) {
	assert.Equal(t, "matrix.org", serverPart("@alice:matrix.org"))
	assert.Equal(t, "matrix.org", serverPart("@DIESEL:matrix.org"))
	assert.Equal(t, "", serverPart("@alice"))
	assert.Equal(t, "", serverPart("@alice:"))
	assert.Equal(t, "", serverPart(""))
}

// TestParseOrigin — the dispatch loop routes replies off the hub
// Origin. Only "matrix:!..." origins are ours; desktop/web/sms/
// telegram turns must not be mistaken for Matrix ones. Room IDs start
// with '!' and contain a colon, but parseOrigin only requires the
// prefix and a non-empty remainder.
func TestParseOrigin(t *testing.T) {
	r, ok := parseOrigin("matrix:!abc:matrix.org")
	assert.True(t, ok)
	assert.Equal(t, id.RoomID("!abc:matrix.org"), r)

	for _, bad := range []string{"desktop", "sms:+15551234567", "telegram:123", "matrix:", ""} {
		_, ok := parseOrigin(bad)
		assert.False(t, ok, bad)
	}
}
