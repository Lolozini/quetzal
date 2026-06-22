// Package ratelimit provides a small in-memory fixed-window rate limiter used to
// slow brute-force attacks on authentication endpoints. It is per-process: with
// multiple apiserver replicas each holds its own counters, which is acceptable
// for the homelab single-replica default (a distributed limiter is future work).
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
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
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
	l.mu.Lock()
	for k, c := range l.hits {
		if now.After(c.reset) {
			delete(l.hits, k)
		}
	}
	l.mu.Unlock()
}
