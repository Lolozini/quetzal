package api_test

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/ratelimit"
)

func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// Ten wrong passwords for an account, from anywhere, used to block it for
// everyone for fifteen minutes: anyone who could reach the panel could keep its
// administrator out, forty requests an hour sufficing to keep them out for
// good. A browser that has signed in to the account now keeps its own count.
func TestLockoutSparesBrowsersThatSignedInBefore(t *testing.T) {
	ts, owner := securedServer(t, func(s *api.Server) {
		s.LoginLimiter = ratelimit.New(3, time.Minute)
		s.DeviceLimiter = ratelimit.New(3, time.Minute)
		s.AuthIPLimiter = ratelimit.New(100, time.Minute) // don't let the address cap interfere
	})
	post(t, owner, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	login := func(c *http.Client, password string) int {
		return post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": password}).StatusCode
	}

	stranger := browser()
	for i := 0; i < 3; i++ {
		if code := login(stranger, "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("stranger attempt %d = %d, want 401", i+1, code)
		}
	}
	// Browsers that never signed in to the account share its count: the
	// stranger is held back even with the right password.
	if code := login(stranger, "supersecret"); code != http.StatusTooManyRequests {
		t.Fatalf("stranger with the right password = %d, want 429", code)
	}
	if code := login(browser(), "supersecret"); code != http.StatusTooManyRequests {
		t.Fatalf("another new browser = %d, want 429", code)
	}
	// The browser the account was set up in is not.
	if code := login(owner, "supersecret"); code != http.StatusOK {
		t.Fatalf("owner = %d, want 200: someone else's failures lock the owner out", code)
	}
	// It has a count of its own, which holds back a thief of the cookie too.
	for i := 0; i < 3; i++ {
		login(owner, "wrong")
	}
	if code := login(owner, "supersecret"); code != http.StatusTooManyRequests {
		t.Errorf("owner after its own failures = %d, want 429", code)
	}
}

// A browser is known under the password it signed in with. Changing it is
// what someone does when they fear it leaked, so the others become strangers;
// the browser it was changed from stays known.
func TestNewPasswordForgetsKnownBrowsers(t *testing.T) {
	ts, owner := securedServer(t, func(s *api.Server) {
		s.LoginLimiter = ratelimit.New(3, time.Minute)
		s.AuthIPLimiter = ratelimit.New(100, time.Minute)
	})
	post(t, owner, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	login := func(c *http.Client, password string) int {
		return post(t, c, ts.URL+"/api/login", map[string]string{"username": "admin", "password": password}).StatusCode
	}
	laptop := browser()
	if code := login(laptop, "supersecret"); code != http.StatusOK {
		t.Fatalf("laptop login = %d", code)
	}
	if r := post(t, owner, ts.URL+"/api/me/password", map[string]string{
		"oldPassword": "supersecret", "newPassword": "brandnewsecret",
	}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("change password = %d", r.StatusCode)
	}
	stranger := browser()
	for i := 0; i < 3; i++ {
		login(stranger, "wrong")
	}
	if code := login(owner, "brandnewsecret"); code != http.StatusOK {
		t.Errorf("browser that changed the password = %d, want 200", code)
	}
	if code := login(laptop, "brandnewsecret"); code != http.StatusTooManyRequests {
		t.Errorf("browser known under the old password = %d, want 429", code)
	}
}

// Signing in used to clear the address's count, so someone with an account of
// their own could sign in to it between two volleys at other accounts and
// never reach the per-address cap.
func TestSignInKeepsTheAddressCount(t *testing.T) {
	ts, admin := securedServer(t, func(s *api.Server) {
		s.LoginLimiter = ratelimit.New(100, time.Minute)
		s.AuthIPLimiter = ratelimit.New(4, time.Minute)
	})
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, ts.URL, map[string]any{"username": "mallory", "password": "mallorypass"})

	c := browser()
	login := func(user, password string) int {
		return post(t, c, ts.URL+"/api/login", map[string]string{"username": user, "password": password}).StatusCode
	}
	login("admin", "guess1")
	login("admin", "guess2")
	if code := login("mallory", "mallorypass"); code != http.StatusOK {
		t.Fatalf("own account = %d, want 200", code)
	}
	if code := login("admin", "guess3"); code != http.StatusUnauthorized {
		t.Fatalf("4th attempt = %d, want 401", code)
	}
	if code := login("admin", "guess4"); code != http.StatusTooManyRequests {
		t.Errorf("5th attempt = %d, want 429: the sign-in cleared the address's count", code)
	}
}

// One IPv6 subscriber commonly holds a whole /64: counted address by address,
// it could draw a fresh budget from each of them.
func TestIPv6AddressesCountByPrefix(t *testing.T) {
	ts, c := securedServer(t, func(s *api.Server) {
		s.TrustProxy = true
		s.AuthIPLimiter = ratelimit.New(2, time.Minute)
	})
	from := func(addr string) int {
		return postWithHeaders(t, c, ts.URL+"/api/login",
			map[string]string{"username": "x", "password": "y"},
			map[string]string{"X-Forwarded-For": addr}).StatusCode
	}
	from("2001:db8:0:1::1")
	from("2001:db8:0:1::2")
	if code := from("2001:db8:0:1:ffff::3"); code != http.StatusTooManyRequests {
		t.Errorf("third address of the /64 = %d, want 429", code)
	}
	if code := from("2001:db8:0:2::1"); code == http.StatusTooManyRequests {
		t.Error("another /64 was held back too")
	}
	if code := from("192.0.2.1"); code == http.StatusTooManyRequests {
		t.Error("an IPv4 address was held back too")
	}
}
