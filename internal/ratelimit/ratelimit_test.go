package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := NewLimiter(0.001, 3, 100)
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("request %d should pass", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("burst exceeded but allowed")
	}
	if !l.Allow("b") {
		t.Fatal("independent key limited")
	}
}

func TestFailuresBackoff(t *testing.T) {
	f := NewFailures(3, time.Hour, time.Minute, 10*time.Minute, 100)
	now := time.Unix(0, 0)
	f.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		f.Fail("k")
	}
	if locked, _ := f.Locked("k"); locked {
		t.Fatal("locked before threshold")
	}
	f.Fail("k")
	locked, until := f.Locked("k")
	if !locked || until.Sub(now) != time.Minute {
		t.Fatalf("want 1m lockout, got %v %v", locked, until.Sub(now))
	}
	f.Fail("k")
	_, until = f.Locked("k")
	if until.Sub(now) != 2*time.Minute {
		t.Fatalf("want doubled lockout, got %v", until.Sub(now))
	}
	for i := 0; i < 10; i++ {
		f.Fail("k")
	}
	_, until = f.Locked("k")
	if until.Sub(now) != 10*time.Minute {
		t.Fatalf("want capped lockout, got %v", until.Sub(now))
	}
	now = now.Add(11 * time.Minute)
	if locked, _ := f.Locked("k"); locked {
		t.Fatal("lockout did not expire")
	}
	f.Succeed("k")
	f.Fail("k")
	if locked, _ := f.Locked("k"); locked {
		t.Fatal("success should reset")
	}
}
