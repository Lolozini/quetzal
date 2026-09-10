package store

import (
	"testing"
	"time"
)

func TestRateAllowCountsAndRollsOver(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	const limit = 3
	window := time.Minute

	for i := 1; i <= limit; i++ {
		ok, reset, err := s.RateAllow("k", limit, window, now)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("attempt %d of %d was refused", i, limit)
		}
		if !reset.After(now) {
			t.Errorf("attempt %d: window ends at %v, not after %v", i, reset, now)
		}
	}
	ok, _, err := s.RateAllow("k", limit, window, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("attempt %d was allowed past a limit of %d", limit+1, limit)
	}
	// A different key has its own budget.
	if ok, _, _ := s.RateAllow("other", limit, window, now); !ok {
		t.Error("a different key was refused")
	}
	// Past the window, the budget is fresh.
	if ok, _, _ := s.RateAllow("k", limit, window, now.Add(window+time.Second)); !ok {
		t.Error("the window did not roll over")
	}
}

func TestRateResetClearsTheBudget(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	for i := 0; i < 2; i++ {
		s.RateAllow("k", 2, time.Minute, now)
	}
	if ok, _, _ := s.RateAllow("k", 2, time.Minute, now); ok {
		t.Fatal("setup: the key should be at its limit")
	}
	if err := s.RateReset("k"); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := s.RateAllow("k", 2, time.Minute, now); !ok {
		t.Error("the key is still refused after a reset")
	}
}

// A refused attempt must not push the window out, or a client that keeps trying
// while blocked would never be unblocked.
func TestRefusedAttemptsDoNotExtendTheWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	window := time.Minute
	_, first, _ := s.RateAllow("k", 1, window, now)
	for i := 0; i < 5; i++ {
		s.RateAllow("k", 1, window, now.Add(time.Duration(i)*time.Second))
	}
	_, after, _ := s.RateAllow("k", 1, window, now.Add(10*time.Second))
	if !after.Equal(first) {
		t.Errorf("window moved from %v to %v while the key was blocked", first, after)
	}
}

func TestRateGCDropsRolledOverWindows(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	s.RateAllow("old", 1, time.Minute, now.Add(-2*time.Minute))
	s.RateAllow("live", 1, time.Minute, now)
	if err := s.RateGC(now); err != nil {
		t.Fatal(err)
	}
	var n int64
	s.db.Table("rate_counters").Count(&n)
	if n != 1 {
		t.Errorf("after GC there are %d counters, want only the live one", n)
	}
}
