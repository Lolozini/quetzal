// Package ratelimit provides a fixed-window rate limiter used to slow
// brute-force attacks on authentication endpoints.
//
// Counters live in memory by default, which has two consequences worth knowing:
// they reset when the process does -- an upgrade hands an attacker a fresh
// budget -- and each apiserver replica holds its own, so the effective limit is
// the configured one times the replica count. Give a limiter a Backend and the
// counters move to shared storage, where neither is true.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter counts attempts per key within a fixed window and blocks once a key
// exceeds the limit until its window rolls over.
type Limiter struct {
	mu     sync.Mutex
	hits   map[string]*counter
	limit  int
	window time.Duration

	backend Backend
	prefix  string
}

// Backend is shared storage for counters, so they outlive the process and are
// seen by every replica. Implemented by store.Store.
type Backend interface {
	RateAllow(key string, limit int, window time.Duration, now time.Time) (bool, time.Time, error)
	RateReset(key string) error
	RateGC(now time.Time) error
}

// Share moves this limiter's counting to b. The prefix keeps limiters apart in
// the shared namespace, so a username and an IP cannot collide on one key.
//
// A backend that errors is not a way in: the call falls back to the in-memory
// counters, which still limit, just per-process as before.
func (l *Limiter) Share(b Backend, prefix string) *Limiter {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	l.backend, l.prefix = b, prefix
	l.mu.Unlock()
	return l
}

type counter struct {
	n     int
	reset time.Time
}

// New returns a limiter allowing limit attempts per window per key. A limit <= 0
// disables limiting (Allow always returns true).
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{hits: map[string]*counter{}, limit: limit, window: window}
}

// Allow records an attempt for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	now := time.Now()
	if b, prefix := l.sharing(); b != nil {
		ok, reset, err := b.RateAllow(prefix+key, l.limit, l.window, now)
		if err == nil {
			// Keep the window locally so RetryAfter answers without a query.
			l.mu.Lock()
			l.hits[key] = &counter{n: 0, reset: reset}
			l.mu.Unlock()
			return ok
		}
		// Storage is unreachable; fall through and limit in memory.
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.hits[key]
	if c == nil || now.After(c.reset) {
		l.hits[key] = &counter{n: 1, reset: now.Add(l.window)}
		return true
	}
	if c.n >= l.limit {
		return false
	}
	c.n++
	return true
}

// Reset clears a key's counter, e.g. after a successful authentication so that
// earlier failures don't count against a legitimate user.
func (l *Limiter) Reset(key string) {
	if l == nil {
		return
	}
	if b, prefix := l.sharing(); b != nil {
		_ = b.RateReset(prefix + key)
	}
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}

// sharing returns the backend and prefix, read under the lock.
func (l *Limiter) sharing() (Backend, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.backend, l.prefix
}

// RetryAfter returns the whole seconds until key's window resets (>= 1 when
// currently blocked, 0 otherwise). Suitable for a Retry-After header.
func (l *Limiter) RetryAfter(key string) int {
	if l == nil || l.limit <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.hits[key]
	if c == nil {
		return 0
	}
	d := time.Until(c.reset)
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}

// GC drops expired counters to bound memory. Safe to call periodically.
func (l *Limiter) GC() {
	if l == nil {
		return
	}
	now := time.Now()
	if b, _ := l.sharing(); b != nil {
		_ = b.RateGC(now)
	}
	l.mu.Lock()
	for k, c := range l.hits {
		if now.After(c.reset) {
			delete(l.hits, k)
		}
	}
	l.mu.Unlock()
}
