package safefetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBlockedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true, // loopback
		"::1":             true, // loopback v6
		"10.1.2.3":        true, // private
		"192.168.0.1":     true, // private
		"172.16.5.5":      true, // private
		"169.254.169.254": true, // link-local / cloud metadata
		"0.0.0.0":         true, // unspecified
		"fe80::1":         true, // link-local v6
		"fc00::1":         true, // unique-local v6 (private)
		// IPv4-mapped IPv6 — a classic SSRF-bypass class; must still be blocked.
		"::ffff:127.0.0.1":       true,
		"::ffff:169.254.169.254": true,
		"::ffff:10.0.0.1":        true,
		"8.8.8.8":                false, // public
		"1.1.1.1":                false, // public
		"93.184.216.34":          false, // public
	}
	for ipStr, want := range cases {
		if got := blockedIP(net.ParseIP(ipStr)); got != want {
			t.Errorf("blockedIP(%s) = %v, want %v", ipStr, got, want)
		}
	}
}

func TestGetRejectsNonHTTPScheme(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "ftp://host/x", "gopher://x"} {
		if _, err := Get(context.Background(), u, 1024); err == nil {
			t.Errorf("Get(%q) should be rejected", u)
		}
	}
}

// TestGetBlocksLoopback is the core SSRF assertion: a real fetch against a
// loopback server (what an attacker would target for internal services / the
// cloud metadata endpoint) must be refused at dial time.
func TestGetBlocksLoopback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("SECRET"))
	}))
	defer ts.Close()

	body, err := Get(context.Background(), ts.URL, 1<<20)
	if err == nil {
		t.Fatalf("Get(%s) must be blocked (loopback), got body %q", ts.URL, body)
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Errorf("error = %v, want a non-public-address refusal", err)
	}
}
