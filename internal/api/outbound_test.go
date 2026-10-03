package api_test

import (
	"net/http"
	"testing"
)

// Inspecting a Pterodactyl server has the panel call an address the caller
// gives: the recette of 0.10.0 did it from an account that may create no
// server, without limit (R-24). Such an account is refused; the others, and
// test mails, are counted.
func TestOutboundRequestsAreForWhoMayMakeThemAndCounted(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret", "email": "admin@example.com"})
	createUser(t, admin, srv.URL, map[string]any{"username": "closed", "password": "closedpw1", "maxServers": 0})
	closed := loginAs(t, srv.URL, "closed", "closedpw1")

	// A loopback panel, which the address guard refuses before any connection:
	// the count is what is tested, not the call.
	body := map[string]any{"url": "https://127.0.0.1", "apiKey": "ptlc_x", "server": "abcd1234"}
	if r := post(t, closed, srv.URL+"/api/import/pterodactyl/inspect", body); r.StatusCode != http.StatusForbidden {
		t.Errorf("inspect from an account that may create no server = %d, want 403", r.StatusCode)
	}

	for i := 0; i < 30; i++ {
		post(t, admin, srv.URL+"/api/import/pterodactyl/inspect", body).Body.Close()
	}
	if r := post(t, admin, srv.URL+"/api/import/pterodactyl/inspect", body); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the 31st inspection in an hour = %d, want 429", r.StatusCode)
	}

	for i := 0; i < 10; i++ {
		post(t, admin, srv.URL+"/api/email-settings/test", map[string]string{"to": "someone@example.com"}).Body.Close()
	}
	if r := post(t, admin, srv.URL+"/api/email-settings/test", map[string]string{"to": "someone@example.com"}); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the 11th test mail in an hour = %d, want 429", r.StatusCode)
	}
}
