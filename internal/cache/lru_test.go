package cache

import (
	"testing"
	"time"
)

func TestLRUEviction(t *testing.T) {
	c := New[string, int](2)
	c.Add("a", 1)
	c.Add("b", 2)
	c.Get("a") // a is now most recent
	c.Add("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatal("a should remain")
	}
}

func TestLRUExpiry(t *testing.T) {
	c := New[string, int](10)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	c.AddTTL("a", 1, time.Minute)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("fresh entry missing")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry returned")
	}
}
