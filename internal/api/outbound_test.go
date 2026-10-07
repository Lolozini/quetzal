package api_test

import (
	"net/http"
	"testing"
)

// A test mail has the panel send a message to an address the caller gives:
// the recette of 0.10.0 found such a call made without limit (R-24). They are
// counted, by account.
func TestOutboundRequestsAreCounted(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret", "email": "admin@example.com"})

	for i := 0; i < 10; i++ {
		post(t, admin, srv.URL+"/api/email-settings/test", map[string]string{"to": "someone@example.com"}).Body.Close()
	}
	if r := post(t, admin, srv.URL+"/api/email-settings/test", map[string]string{"to": "someone@example.com"}); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the 11th test mail in an hour = %d, want 429", r.StatusCode)
	}
}
