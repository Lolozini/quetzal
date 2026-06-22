package ratelimit

import (
	"testing"
	"time"
)

func TestAllowBlocksAfterLimit(t *testing.T) {
	l := New(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.Allow("a") {
		t.Error("4th attempt should be blocked")
	}
	// A different key is independent.
	if !l.Allow("b") {
		t.Error("other key should be allowed")
	}
	if ra := l.RetryAfter("a"); ra <= 0 || ra > 60 {
		t.Errorf("RetryAfter = %d, want 1..60", ra)
	}
}

func TestResetClearsCounter(t *testing.T) {
	l := New(2, time.Minute)
	l.Allow("a")
	l.Allow("a")
	if l.Allow("a") {
		t.Fatal("should be blocked before reset")
	}
	l.Reset("a")
	if !l.Allow("a") {
		t.Error("should be allowed after reset")
	}
}

func TestWindowRollsOver(t *testing.T) {
	l := New(1, 20*time.Millisecond)
	if !l.Allow("a") {
		t.Fatal("first allowed")
	}
	if l.Allow("a") {
		t.Fatal("second blocked within window")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("a") {
		t.Error("should be allowed after the window rolls over")
	}
}

func TestZeroLimitDisables(t *testing.T) {
	l := New(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !l.Allow("a") {
			t.Fatal("limit<=0 must never block")
		}
	}
	var nilL *Limiter
	if !nilL.Allow("a") {
		t.Error("nil limiter must allow")
	}
}

func TestGCDropsExpired(t *testing.T) {
	l := New(1, 10*time.Millisecond)
	l.Allow("a")
	time.Sleep(20 * time.Millisecond)
	l.GC()
	l.mu.Lock()
	n := len(l.hits)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("GC should have dropped expired counters, %d remain", n)
	}
}
