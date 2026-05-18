package hub

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	h := New()
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
	h := New()
	first := h.Subscribe("same-id")
	second := h.Subscribe("same-id")

	_, ok := <-first.Events
	assert.False(t, ok, "first subscriber's channel must be closed when replaced")
	assert.NotNil(t, second.Events)
}

// TestComposeImagePrompt covers the three splice paths — clothing
// when dressed, nudity when naked, emotion always.
func TestComposeImagePrompt(t *testing.T) {
	s := settingsFixture()

	got := composeImagePrompt(s, "happy", false)
	assert.Contains(t, got, "BASE")
	assert.Contains(t, got, "CLOTHING")
	assert.Contains(t, got, "warm smile")
	assert.NotContains(t, got, "NUDE")

	got = composeImagePrompt(s, "happy", true)
	assert.Contains(t, got, "NUDE")
	assert.NotContains(t, got, "CLOTHING")

	got = composeImagePrompt(s, "neutral", false)
	// neutral emotion = empty splice, prompt stays minimal
	assert.Equal(t, "BASE, CLOTHING", got)
}
