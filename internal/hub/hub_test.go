package hub

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diesel/internal/comfyui"
	"diesel/internal/settings"
	"diesel/internal/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore opens a throwaway SQLite database in a temp dir, closed
// automatically when the test ends. It also wires the settings backend
// to an in-memory stub seeded with valid identity fields — hub.Send
// refuses to dispatch otherwise, which would defeat tests that aren't
// about identity gating.
func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "diesel.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	stub := settings.AppSettings{
		FirstName: "Alex",
		LastName:  "Doe",
		PetName:   "Mittens",
	}
	settings.SetBackend(
		func() settings.AppSettings { return stub },
		func(s settings.AppSettings) error { stub = s; return nil },
	)
	t.Cleanup(func() { settings.SetBackend(nil, nil) })
	return st
}

// TestBlobCache_FIFOEviction exercises the eviction order — the hub
// keeps only the last N media blobs and the rest are dropped. Order
// matters because the gin handlers serve by ID and a stale fetch
// should 404 cleanly rather than returning the wrong blob.
func TestBlobCache_FIFOEviction(t *testing.T) {
	c := newBlobCache(3)
	c.put("a", []byte("A"))
	c.put("b", []byte("B"))
	c.put("c", []byte("C"))
	c.put("d", []byte("D"))

	_, ok := c.get("a")
	assert.False(t, ok, "oldest entry should be evicted")
	for _, id := range []string{"b", "c", "d"} {
		v, ok := c.get(id)
		require.True(t, ok, "id %q should still be present", id)
		assert.Equal(t, []byte(strings.ToUpper(id)), v)
	}
	id, data := c.latest()
	assert.Equal(t, "d", id)
	assert.Equal(t, []byte("D"), data)
}

// TestBlobCache_PutSameIDReplaces — re-putting an existing ID should
// update the value without changing eviction order.
func TestBlobCache_PutSameIDReplaces(t *testing.T) {
	c := newBlobCache(2)
	c.put("a", []byte("1"))
	c.put("b", []byte("2"))
	c.put("a", []byte("3")) // replace, not insert
	c.put("c", []byte("4")) // this should evict "b" not "a"

	_, ok := c.get("b")
	assert.False(t, ok, "b should have been evicted")
	v, ok := c.get("a")
	require.True(t, ok)
	assert.Equal(t, []byte("3"), v)
}

// TestSubscribeUnsubscribe — basic registration and the
// close-on-unsubscribe contract.
func TestSubscribeUnsubscribe(t *testing.T) {
	h := New(newTestStore(t))
	sub := h.Subscribe("test")
	assert.Equal(t, "test", sub.ID)

	h.Unsubscribe("test")
	_, ok := <-sub.Events
	assert.False(t, ok, "events channel should be closed after unsubscribe")
}

// TestSubscribe_ReplaceClosesPrior — a reconnecting WS client reuses
// its ID; the prior subscriber's channel must close so its goroutine
// exits.
func TestSubscribe_ReplaceClosesPrior(t *testing.T) {
	h := New(newTestStore(t))
	first := h.Subscribe("same-id")
	second := h.Subscribe("same-id")

	_, ok := <-first.Events
	assert.False(t, ok, "first subscriber's channel must be closed when replaced")
	assert.NotNil(t, second.Events)
}

// TestSend_DetachesCallerContext is a regression for the bug where
// the goroutine inherited an HTTP handler's context: when the handler
// returned (right after Send), the context canceled and the LLM HTTP
// call inside runTurn died mid-flight with "context canceled". Send
// must run runTurn with a context that ignores the caller's cancel.
//
// We can't drive a real LLM here, but we can assert the contract: an
// EventTurnStarted (or EventTurnError, when there's no endpoint
// configured) arrives at the subscriber *after* the caller's context
// was canceled — proving the pipeline isn't gated on the caller's
// context. The error path is fine for this; we're verifying the
// goroutine got far enough to attempt and report, not that the LLM
// itself succeeded.
func TestSend_DetachesCallerContext(t *testing.T) {
	h := New(newTestStore(t))
	sub := h.Subscribe("test")

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.Send(ctx, "hi", "test", false))
	// Cancel immediately — mirrors a gin handler returning right
	// after kicking off the turn.
	cancel()

	// Drain events. We expect EventTurnStarted (sync, before goroutine
	// runs) and then EventTurnError (the goroutine tried to call the
	// LLM, failed because no endpoint is configured — but crucially
	// not because of "context canceled").
	deadline := time.After(3 * time.Second)
	var sawStarted, sawError bool
	var errMsg string
	for !sawStarted || !sawError {
		select {
		case ev, ok := <-sub.Events:
			if !ok {
				t.Fatal("subscriber channel closed before terminal event")
			}
			switch ev.Type {
			case EventTurnStarted:
				sawStarted = true
			case EventTurnError:
				sawError = true
				errMsg = ev.Error
			}
		case <-deadline:
			t.Fatalf("timed out (started=%v error=%v)", sawStarted, sawError)
		}
	}
	// The whole point of the test: the failure mode must not be the
	// caller-cancel one we used to hit. Any other LLM/config error
	// is fine — that's not what we're guarding against here.
	assert.NotContains(t, errMsg, "context canceled",
		"runTurn inherited caller's canceled context")
}

// TestComposeImagePrompt covers the splice paths: quality prefix and
// character always present; pose-base + pose-addon; background; clothing
// vs nudity; emotion always (no-op on neutral). The matrix lookups are
// the load-bearing part — a missing pose/background slug must not crash,
// and the splice fragments must land in the prompt verbatim.
func TestComposeImagePrompt(t *testing.T) {
	got := composeImagePrompt("happy", "living_room", "standing", false)
	assert.Contains(t, got, comfyui.ImageQualityPrefix)
	assert.Contains(t, got, comfyui.ImagePrompt)
	assert.Contains(t, got, comfyui.ImagePoseBases["standing"].Tags)
	assert.Contains(t, got, comfyui.ImagePoseAddons["standing"]["living_room"])
	assert.Contains(t, got, comfyui.ImageBackgrounds["living_room"].Tags)
	assert.Contains(t, got, comfyui.ImageClothing)
	assert.Contains(t, got, "warm smile")
	assert.NotContains(t, got, comfyui.ImageNudity)

	got = composeImagePrompt("happy", "pub", "sitting", true)
	assert.Contains(t, got, comfyui.ImagePoseBases["sitting"].Tags)
	assert.Contains(t, got, comfyui.ImagePoseAddons["sitting"]["pub"])
	assert.Contains(t, got, comfyui.ImageBackgrounds["pub"].Tags)
	assert.Contains(t, got, comfyui.ImageNudity)
	assert.NotContains(t, got, comfyui.ImageClothing)

	// Neutral emotion contributes no expression tags, but every other
	// splice still fires — verify by reconstructing the expected string
	// in the documented order.
	got = composeImagePrompt("neutral", "forest_park", "bent_over", false)
	want := comfyui.ImageQualityPrefix +
		", " + comfyui.ImagePrompt +
		", " + comfyui.ImagePoseBases["bent_over"].Tags +
		", " + comfyui.ImagePoseAddons["bent_over"]["forest_park"] +
		", " + comfyui.ImageBackgrounds["forest_park"].Tags +
		", " + comfyui.ImageClothing
	assert.Equal(t, want, got)

	// Unknown slugs no-op gracefully so older saved conversations (and
	// any malformed reply that slips past the schema) still produce a
	// renderable prompt rather than panicking on a nil-map lookup.
	got = composeImagePrompt("neutral", "atlantis", "moonwalk", false)
	assert.Contains(t, got, comfyui.ImageQualityPrefix)
	assert.Contains(t, got, comfyui.ImagePrompt)
	assert.Contains(t, got, comfyui.ImageClothing)
	assert.NotContains(t, got, "scenery")
}

// TestComposeImagePrompt_TagOrder verifies the Booru-convention ordering
// is preserved. Drift here means the renderer sees the subject after the
// scene (weaker character lock) or the emotion before the clothing (which
// can blend expression into the outfit). Asserts by index lookup so it
// fails noisily rather than just flagging a missing tag.
func TestComposeImagePrompt_TagOrder(t *testing.T) {
	got := composeImagePrompt("amused", "mechanics_shop", "standing", false)
	idx := func(needle string) int { return strings.Index(got, needle) }
	require.NotEqual(t, -1, idx(comfyui.ImageQualityPrefix))
	require.NotEqual(t, -1, idx(comfyui.ImagePrompt))
	require.NotEqual(t, -1, idx(comfyui.ImagePoseBases["standing"].Tags))
	require.NotEqual(t, -1, idx(comfyui.ImageBackgrounds["mechanics_shop"].Tags))
	require.NotEqual(t, -1, idx(comfyui.ImageClothing))
	require.NotEqual(t, -1, idx("amused expression"))

	assert.Less(t, idx(comfyui.ImageQualityPrefix), idx(comfyui.ImagePrompt))
	assert.Less(t, idx(comfyui.ImagePrompt), idx(comfyui.ImagePoseBases["standing"].Tags))
	assert.Less(t, idx(comfyui.ImagePoseBases["standing"].Tags), idx(comfyui.ImageBackgrounds["mechanics_shop"].Tags))
	assert.Less(t, idx(comfyui.ImageBackgrounds["mechanics_shop"].Tags), idx(comfyui.ImageClothing))
	assert.Less(t, idx(comfyui.ImageClothing), idx("amused expression"))
}
