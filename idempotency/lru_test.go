package idempotency

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLocalCache(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	completed := func(key string) Record {
		return Record{Key: key, Status: StatusCompleted, ExpiresAt: now.Add(time.Minute)}
	}
	c := newLocalCache(2)

	c.put(completed("a"))
	c.put(completed("b"))
	_, ok := c.get("a", now) // a becomes the most recently used
	assert.True(t, ok)
	c.put(completed("c")) // evicts b
	_, ok = c.get("b", now)
	assert.False(t, ok, "b was the least recently used")
	_, ok = c.get("a", now)
	assert.True(t, ok)

	c.put(Record{Key: "d", Status: StatusInProgress, ExpiresAt: now.Add(time.Minute)})
	_, ok = c.get("d", now)
	assert.False(t, ok, "in-progress records are never cached")

	c.put(completed("c")) // an update keeps one entry
	assert.Equal(t, 2, c.order.Len())

	_, ok = c.get("c", now.Add(time.Minute+time.Second))
	assert.False(t, ok, "expired records are dropped")
	assert.Equal(t, 1, c.order.Len())

	c.remove("a")
	_, ok = c.get("a", now)
	assert.False(t, ok)
}

func TestNilLocalCacheIsOff(t *testing.T) {
	t.Parallel()
	var c *localCache
	c.put(Record{Key: "a", Status: StatusCompleted})
	_, ok := c.get("a", time.Now())
	assert.False(t, ok)
	c.remove("a")
}

func TestRecordHoldsKey(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(1_000_000_500)
	cases := []struct {
		name   string
		record Record
		holds  bool
	}{
		{"completed", Record{Status: StatusCompleted, ExpiresAt: now.Add(time.Second)}, true},
		{"completed in its last second", Record{Status: StatusCompleted, ExpiresAt: time.Unix(now.Unix(), 0)}, true},
		{"expired", Record{Status: StatusCompleted, ExpiresAt: time.Unix(now.Unix()-1, 0)}, false},
		{"in progress", Record{Status: StatusInProgress, ExpiresAt: now.Add(time.Hour), InProgressExpiresAt: now.Add(time.Millisecond)}, true},
		{"in progress at its deadline", Record{Status: StatusInProgress, ExpiresAt: now.Add(time.Hour), InProgressExpiresAt: now}, true},
		{"timed out", Record{Status: StatusInProgress, ExpiresAt: now.Add(time.Hour), InProgressExpiresAt: now.Add(-time.Millisecond)}, false},
		{"in progress without deadline", Record{Status: StatusInProgress, ExpiresAt: now.Add(time.Hour)}, true},
		{"unknown status", Record{Status: "DONE", ExpiresAt: now.Add(time.Hour)}, true},
		{"no expiry", Record{Status: StatusCompleted}, true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.holds, tc.record.HoldsKey(now), tc.name)
	}
}
