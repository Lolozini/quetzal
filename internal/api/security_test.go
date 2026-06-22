package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/ratelimit"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

// securedServer builds an api.Server, lets the caller tune it (e.g. tighten rate
// limits), and serves it.
func securedServer(t *testing.T, tune func(*api.Server)) (*httptest.Server, *http.Client) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "s.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := api.New(st, fake.NewSimpleClientset(), &rest.Config{})
	if tune != nil {
		tune(srv)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar}
}

func postWithHeaders(t *testing.T, c *http.Client, url string, body any, headers map[string]string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func TestLoginRateLimit(t *testing.T) {
	ts, c := securedServer(t, func(s *api.Server) {
		s.LoginLimiter = ratelimit.New(3, time.Minute)
		s.AuthIPLimiter = ratelimit.New(100, time.Minute) // don't let the IP cap interfere
	})
	post(t, c, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	bad := func() int {
		r := post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "wrong"})
		return r.StatusCode
	}
	for i := 0; i < 3; i++ {
		if code := bad(); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, code)
		}
	}
	r := post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "wrong"})
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th attempt = %d, want 429", r.StatusCode)
	}
	if r.Header.Get("Retry-After") == "" {
		t.Error("429 should carry a Retry-After header")
	}
	// Even the correct password is now blocked (lockout) for this username.
	if r := post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret"}); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("locked-out correct password = %d, want 429", r.StatusCode)
	}
}

func TestLoginRateLimitResetsOnSuccess(t *testing.T) {
	ts, c := securedServer(t, func(s *api.Server) {
		s.LoginLimiter = ratelimit.New(3, time.Minute)
		s.AuthIPLimiter = ratelimit.New(100, time.Minute)
	})
	post(t, c, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// Two failures, then a success that clears the counter.
	post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "wrong"})
	post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "wrong"})
	if r := post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret"}); r.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", r.StatusCode)
	}
	// The budget is fresh again: another failure is 401, not 429.
	if r := post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "wrong"}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("post-reset failure = %d, want 401 (counter cleared)", r.StatusCode)
	}
}

func TestCSRFBlocksCrossOrigin(t *testing.T) {
	ts, c := securedServer(t, nil)

	// Cross-origin unsafe request is rejected before processing.
	r := postWithHeaders(t, c, ts.URL+"/api/login",
		map[string]string{"username": "x", "password": "y"},
		map[string]string{"Origin": "http://evil.example"})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", r.StatusCode)
	}
	// Same-origin passes the CSRF gate (and then fails auth, not 403).
	r = postWithHeaders(t, c, ts.URL+"/api/login",
		map[string]string{"username": "x", "password": "y"},
		map[string]string{"Origin": ts.URL})
	if r.StatusCode == http.StatusForbidden {
		t.Error("same-origin POST must not be blocked by CSRF")
	}
	// No Origin (non-browser client) is allowed.
	r = post(t, c, ts.URL+"/api/setup/status", nil)
	if r.StatusCode == http.StatusForbidden {
		t.Error("request without Origin must not be blocked")
	}
	// Safe methods are never blocked, even cross-origin.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/setup/status", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusForbidden {
		t.Error("cross-origin GET must not be blocked")
	}
}

func TestInternalEndpointRateLimit(t *testing.T) {
	ts, c := securedServer(t, func(s *api.Server) {
		s.InternalLimiter = ratelimit.New(2, time.Minute)
	})
	wake := func() int {
		r := post(t, c, ts.URL+"/api/internal/wake", map[string]string{"slug": "nope", "token": "x"})
		return r.StatusCode
	}
	if wake() != http.StatusNoContent || wake() != http.StatusNoContent {
		t.Fatal("first two wake calls should pass (204)")
	}
	if code := wake(); code != http.StatusTooManyRequests {
		t.Errorf("third wake call = %d, want 429", code)
	}
}
