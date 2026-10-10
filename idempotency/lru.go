package idempotency

import (
	"container/list"
	"sync"
	"time"
)

// DefaultLocalCacheMaxItems is the capacity of the local cache unless
// WithLocalCacheMaxItems says otherwise, as in Powertools Python.
const DefaultLocalCacheMaxItems = 256

// localCache keeps the COMPLETED records an execution environment has seen,
// so that a replay does not read the store. It evicts the least recently
// used record beyond its capacity. Unlike Python's, it is safe for
// concurrent invocations.
type localCache struct {
	mu       sync.Mutex
	maxItems int
	order    *list.List // of Record, most recently used at the back
	entries  map[string]*list.Element
}

func newLocalCache(maxItems int) *localCache {
	return &localCache{maxItems: maxItems, order: list.New(), entries: make(map[string]*list.Element)}
}

// get returns the record for key, unless it is missing or expired at now; an
// expired one is dropped.
func (c *localCache) get(key string, now time.Time) (Record, bool) {
	if c == nil {
		return Record{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return Record{}, false
	}
	record, _ := element.Value.(Record) //nolint:errcheck // the list only ever holds records
	if record.IsExpired(now) {
		c.order.Remove(element)
		delete(c.entries, key)
		return Record{}, false
	}
	c.order.MoveToBack(element)
	return record, true
}

// put stores record, unless it is INPROGRESS: another environment may
// complete or delete it, and nothing would tell this cache.
func (c *localCache) put(record Record) {
	if c == nil || record.Status == StatusInProgress {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[record.Key]; ok {
		element.Value = record
		c.order.MoveToBack(element)
		return
	}
	c.entries[record.Key] = c.order.PushBack(record)
	if c.order.Len() > c.maxItems {
		oldest := c.order.Front()
		c.order.Remove(oldest)
		if evicted, ok := oldest.Value.(Record); ok {
			delete(c.entries, evicted.Key)
		}
	}
}

// remove drops the record for key.
func (c *localCache) remove(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		c.order.Remove(element)
		delete(c.entries, key)
	}
}
