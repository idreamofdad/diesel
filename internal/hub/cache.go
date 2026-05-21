package hub

import (
	"os"
	"strings"
)

// blobCache is a tiny LRU-ish cache keyed by string ID, used to hold the
// most recent N audio / portrait blobs the hub has broadcast. Eviction
// is insertion-order (FIFO) rather than true LRU — clients fetch via
// HTTP within seconds of the broadcast, so the difference doesn't
// matter. Not safe for concurrent use; the hub holds its mutex around
// every call.
type blobCache struct {
	max    int
	order  []string
	values map[string][]byte
}

func newBlobCache(max int) *blobCache {
	if max <= 0 {
		max = 1
	}
	return &blobCache{
		max:    max,
		order:  make([]string, 0, max),
		values: make(map[string][]byte, max),
	}
}

func (c *blobCache) put(id string, data []byte) {
	if _, exists := c.values[id]; exists {
		// Move to the back so a re-put refreshes the entry's recency —
		// otherwise repeated puts of a known ID would let other entries
		// silently age past it.
		for i, k := range c.order {
			if k == id {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		c.order = append(c.order, id)
		c.values[id] = data
		return
	}
	c.values[id] = data
	c.order = append(c.order, id)
	for len(c.order) > c.max {
		evict := c.order[0]
		c.order = c.order[1:]
		delete(c.values, evict)
	}
}

func (c *blobCache) get(id string) ([]byte, bool) {
	v, ok := c.values[id]
	return v, ok
}

// latest returns the most-recently inserted entry — used to serve
// /api/v1/portrait/latest without needing the caller to know the ID.
func (c *blobCache) latest() (string, []byte) {
	if len(c.order) == 0 {
		return "", nil
	}
	id := c.order[len(c.order)-1]
	return id, c.values[id]
}

// trimSpace is a local alias to keep the hub file free of an extra
// strings import — composeImagePrompt is its only caller.
func trimSpace(s string) string { return strings.TrimSpace(s) }

// readFile is a thin wrapper so hub.go can avoid importing os directly
// (keeps the public file's import list short and intentional).
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
