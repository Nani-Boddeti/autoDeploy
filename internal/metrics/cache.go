package metrics

import (
	"context"
	"sync"
	"time"
)

// Cache memoises one expensive fetch (a database snapshot) for a TTL. The
// mutex is held across the fetch, so concurrent scrapes during a refresh share
// one in-flight query instead of stampeding the database; the fetch is bounded
// by its own timeout (independent of any scraper's request context), so
// waiters wait at most that long. A failed fetch is cached for the TTL too, so
// a down database costs at most one timeout per failure TTL, not one per scrape.
type Cache[T any] struct {
	ttl     time.Duration
	failTTL time.Duration
	timeout time.Duration
	fetch   func(context.Context) (T, error)
	onError func(error)

	mu    sync.Mutex
	now   func() time.Time
	have  bool
	at    time.Time
	value T
	ok    bool
}

// NewCache builds a cache. onError (may be nil) is called once per failed
// fetch, with the lock held, so it must be cheap and must not call Get.
func NewCache[T any](ttl, failTTL, timeout time.Duration, fetch func(context.Context) (T, error), onError func(error)) *Cache[T] {
	return &Cache[T]{ttl: ttl, failTTL: failTTL, timeout: timeout, fetch: fetch, onError: onError, now: time.Now}
}

// SetClock replaces the time source (tests only).
func (c *Cache[T]) SetClock(now func() time.Time) { c.now = now }

// Get returns the cached value, refreshing it when older than the TTL. ok is
// false when the latest fetch failed.
func (c *Cache[T]) Get() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := c.now().Sub(c.at)
	if c.have && ((c.ok && age < c.ttl) || (!c.ok && age < c.failTTL)) {
		return c.value, c.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	v, err := c.fetch(ctx)
	c.have, c.at = true, c.now()
	if err != nil {
		var zero T
		c.value, c.ok = zero, false
		if c.onError != nil {
			c.onError(err)
		}
		return c.value, false
	}
	c.value, c.ok = v, true
	return v, true
}
