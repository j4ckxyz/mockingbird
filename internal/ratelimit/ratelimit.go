// Package ratelimit provides keyed token buckets and a login-failure tracker
// with exponential backoff. State is in memory and bounded by an LRU, so a
// flood of distinct keys cannot exhaust memory.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/j4ckxyz/mockingbird/internal/cache"
)

// Limiter is a set of token buckets keyed by string.
type Limiter struct {
	rate    rate.Limit
	burst   int
	buckets *cache.LRU[string, *rate.Limiter]
	mu      sync.Mutex
}

// NewLimiter allows perSecond sustained and burst peak per key.
func NewLimiter(perSecond float64, burst, maxKeys int) *Limiter {
	return &Limiter{rate: rate.Limit(perSecond), burst: burst, buckets: cache.New[string, *rate.Limiter](maxKeys)}
}

// Allow consumes one token for key.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	b, ok := l.buckets.Get(key)
	if !ok {
		b = rate.NewLimiter(l.rate, l.burst)
		l.buckets.Add(key, b)
	}
	l.mu.Unlock()
	return b.Allow()
}

// Failures tracks failed logins per key and imposes growing lockouts.
type Failures struct {
	threshold int           // failures allowed before lockout begins
	window    time.Duration // failures older than this are forgotten
	base      time.Duration // first lockout length; doubles per further failure
	max       time.Duration
	mu        sync.Mutex
	state     *cache.LRU[string, *failState]
	now       func() time.Time
}

type failState struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

// NewFailures builds a tracker.
func NewFailures(threshold int, window, base, max time.Duration, maxKeys int) *Failures {
	return &Failures{threshold: threshold, window: window, base: base, max: max,
		state: cache.New[string, *failState](maxKeys), now: time.Now}
}

// Locked reports whether key is locked out, and until when.
func (f *Failures) Locked(key string) (bool, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state.Get(key)
	if !ok {
		return false, time.Time{}
	}
	now := f.now()
	if now.Before(s.lockedUntil) {
		return true, s.lockedUntil
	}
	return false, time.Time{}
}

// Fail records a failure for key.
func (f *Failures) Fail(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	s, ok := f.state.Get(key)
	if !ok || now.Sub(s.first) > f.window && now.After(s.lockedUntil) {
		s = &failState{first: now}
		f.state.Add(key, s)
	}
	s.count++
	if s.count >= f.threshold {
		d := f.base << min(s.count-f.threshold, 16)
		if d > f.max || d <= 0 {
			d = f.max
		}
		s.lockedUntil = now.Add(d)
	}
}

// Succeed clears key.
func (f *Failures) Succeed(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Remove(key)
}
