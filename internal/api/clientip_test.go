package api

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	s := &Server{}

	r := httptest.NewRequest("POST", "/x", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	// Untrusted: X-Forwarded-For is ignored (spoofable), use the socket address.
	if got := s.clientIP(r); got != "10.0.0.1" {
		t.Errorf("untrusted clientIP = %q, want 10.0.0.1", got)
	}

	// Trusted single-hop proxy: the real client is the RIGHTMOST entry (the one
	// the trusted proxy appended); a client-supplied leftmost value is ignored.
	s.TrustProxy = true
	if got := s.clientIP(r); got != "5.6.7.8" {
		t.Errorf("trusted clientIP = %q, want 5.6.7.8 (rightmost, not the spoofable leftmost)", got)
	}

	// Trusted with a single XFF value.
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := s.clientIP(r); got != "9.9.9.9" {
		t.Errorf("single XFF = %q, want 9.9.9.9", got)
	}

	// Trusted but no XFF: fall back to the socket address (host only).
	r.Header.Del("X-Forwarded-For")
	if got := s.clientIP(r); got != "10.0.0.1" {
		t.Errorf("no XFF clientIP = %q, want 10.0.0.1", got)
	}
}
