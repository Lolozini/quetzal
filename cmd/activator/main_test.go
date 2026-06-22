package main

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestWakerDebounces(t *testing.T) {
	var calls int32
	base := time.Now()
	now := base
	w := &waker{
		cooldown: 15 * time.Second,
		post:     func() error { atomic.AddInt32(&calls, 1); return nil },
		now:      func() time.Time { return now },
	}

	// A burst within the cooldown window fires exactly once.
	for i := 0; i < 5; i++ {
		w.trigger()
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("burst fired %d wakes, want 1", got)
	}

	// After the cooldown elapses, it can fire again.
	now = base.Add(20 * time.Second)
	w.trigger()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("post-cooldown wakes = %d, want 2", got)
	}
}

func TestSplitPorts(t *testing.T) {
	got := splitPorts("25565, 25575 ,, 2456")
	want := []string{"25565", "25575", "2456"}
	if len(got) != len(want) {
		t.Fatalf("splitPorts = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("port[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
