package validation

import "sync"

// cache is a bounded map safe for concurrent use. When full, it forgets the
// oldest entry: a function validates against a handful of schemas, and the
// bound only protects one that builds schemas on the fly from growing
// without limit.
type cache[V any] struct {
	mu      sync.Mutex
	limit   int
	entries map[string]V
	order   []string
}

func newCache[V any](limit int) *cache[V] {
	return &cache[V]{limit: limit, entries: make(map[string]V, limit)}
}

func (c *cache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	return v, ok
}

func (c *cache[V]) put(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		c.entries[key] = v
		return
	}
	if len(c.order) >= c.limit {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.entries[key] = v
	c.order = append(c.order, key)
}
