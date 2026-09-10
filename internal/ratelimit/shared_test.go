package ratelimit_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/ratelimit"
	"github.com/lolozini/quetzal/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "r.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// Two apiserver replicas are two processes with two sets of counters, so a limit
// of 5 used to mean 10. Sharing the counting makes the configured limit the
// limit however many are running.
func TestReplicasShareOneBudget(t *testing.T) {
	st := newStore(t)
	const limit = 5
	a := ratelimit.New(limit, time.Minute).Share(st, "login:")
	b := ratelimit.New(limit, time.Minute).Share(st, "login:")

	allowed := 0
	for i := 0; i < limit*2; i++ {
		if i%2 == 0 {
			if a.Allow("victim") {
				allowed++
			}
		} else if b.Allow("victim") {
			allowed++
		}
	}
	if allowed != limit {
		t.Errorf("two replicas allowed %d attempts against a limit of %d", allowed, limit)
	}
	if a.Allow("victim") || b.Allow("victim") {
		t.Error("a further attempt got through on one of the replicas")
	}
	// Clearing on one is seen by the other, or a successful login on replica A
	// would leave the account still blocked on replica B.
	a.Reset("victim")
	if !b.Allow("victim") {
		t.Error("the other replica still refuses after a reset")
	}
}

// Counters in memory die with the process, so an upgrade -- or a crash an
// attacker can provoke -- handed out a fresh budget.
func TestBudgetSurvivesARestart(t *testing.T) {
	st := newStore(t)
	const limit = 3
	before := ratelimit.New(limit, time.Minute).Share(st, "login:")
	for i := 0; i < limit; i++ {
		before.Allow("victim")
	}
	if before.Allow("victim") {
		t.Fatal("setup: the key should be at its limit")
	}
	// A new process, same storage.
	after := ratelimit.New(limit, time.Minute).Share(st, "login:")
	if after.Allow("victim") {
		t.Error("restarting the process reset the brute-force counter")
	}
	if ra := after.RetryAfter("victim"); ra <= 0 {
		t.Errorf("RetryAfter = %d after a restart, want a positive wait", ra)
	}
}

// Different limiters share one namespace, so their keys must not collide: a
// username and an IP that happen to read the same must count separately.
func TestPrefixesKeepLimitersApart(t *testing.T) {
	st := newStore(t)
	login := ratelimit.New(1, time.Minute).Share(st, "login:")
	byIP := ratelimit.New(1, time.Minute).Share(st, "ip:")
	if !login.Allow("10.0.0.1") {
		t.Fatal("first login attempt refused")
	}
	if !byIP.Allow("10.0.0.1") {
		t.Error("the IP limiter spent the login limiter's budget")
	}
}

type brokenBackend struct{}

func (brokenBackend) RateAllow(string, int, time.Duration, time.Time) (bool, time.Time, error) {
	return false, time.Time{}, errors.New("storage is down")
}
func (brokenBackend) RateReset(string) error { return errors.New("storage is down") }
func (brokenBackend) RateGC(time.Time) error { return errors.New("storage is down") }

// Storage being unreachable must not become a way in, nor a way to lock everyone
// out: the limiter falls back to counting in memory, as it did before.
func TestUnreachableStorageStillLimits(t *testing.T) {
	const limit = 2
	l := ratelimit.New(limit, time.Minute).Share(brokenBackend{}, "login:")
	allowed := 0
	for i := 0; i < limit+3; i++ {
		if l.Allow("victim") {
			allowed++
		}
	}
	if allowed != limit {
		t.Errorf("with storage down the limiter allowed %d attempts, want %d", allowed, limit)
	}
}
