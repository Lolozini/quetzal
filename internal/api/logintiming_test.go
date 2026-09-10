package api_test

import (
	"net/http"
	"testing"
	"time"
)

// Login used to answer in a millisecond for a username that does not exist and
// in fifty for one that does, because only the second reached argon2. One
// request then told an attacker whether an account existed, and the per-account
// throttle was no help: a single probe is all it takes.
//
// Measured as the minimum of several attempts, which is the stable statistic --
// scheduling noise can only make a run slower, never faster.
func TestLoginDoesNotSayWhetherAnAccountExists(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "known", "password": "knownpw12345"})

	probe := func(user string) time.Duration {
		best := time.Hour
		for i := 0; i < 4; i++ {
			start := time.Now()
			r := post(t, &http.Client{}, srv.URL+"/api/login",
				map[string]string{"username": user, "password": "wrong-password"})
			d := time.Since(start)
			if r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("login as %q = %d, want 401 (a throttled reply would not be timed)", user, r.StatusCode)
			}
			if d < best {
				best = d
			}
		}
		return best
	}
	known, unknown := probe("known"), probe("no-such-account")

	// The unknown path must do comparable work. Half is a wide margin: before the
	// decoy hash it was doing none, and came back ~7x faster.
	if unknown*2 < known {
		t.Errorf("unknown account answered in %v against %v for a known one: the gap still says which usernames exist",
			unknown, known)
	}
}
