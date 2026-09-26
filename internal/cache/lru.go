// Package cache provides a bounded, concurrency-safe LRU with optional
// per-entry expiry.
package cache

import (
	"container/list"
	"sync"
	"time"
)

// LRU is a size-bounded cache. The zero value is not usable; call New.
type LRU[K comparable, V any] struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[K]*list.Element
	now   func() time.Time
}

type entry[K comparable, V any] struct {
	key     K
	val     V
	expires time.Time // zero means never
}

// New returns an LRU holding at most max entries.
func New[K comparable, V any](max int) *LRU[K, V] {
	if max < 1 {
		max = 1
	}
	return &LRU[K, V]{max: max, ll: list.New(), items: make(map[K]*list.Element), now: time.Now}
}

// Get returns the value for k if present and unexpired.
func (c *LRU[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[k]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if !e.expires.IsZero() && c.now().After(e.expires) {
		c.ll.Remove(el)
		delete(c.items, k)
		return zero, false
	}
	c.ll.MoveToFront(el)
	return e.val, true
}

// Add stores v under k without expiry.
func (c *LRU[K, V]) Add(k K, v V) { c.AddTTL(k, v, 0) }

// AddTTL stores v under k, expiring after ttl (0 means never).
func (c *LRU[K, V]) AddTTL(k K, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	if el, ok := c.items[k]; ok {
		e := el.Value.(*entry[K, V])
		e.val, e.expires = v, exp
		c.ll.MoveToFront(el)
		return
	}
	c.items[k] = c.ll.PushFront(&entry[K, V]{key: k, val: v, expires: exp})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.items, old.Value.(*entry[K, V]).key)
	}
}

// Remove deletes k.
func (c *LRU[K, V]) Remove(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.ll.Remove(el)
		delete(c.items, k)
	}
}

// Len returns the number of entries, including expired ones not yet evicted.
func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
