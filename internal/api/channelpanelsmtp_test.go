package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// An email channel needed an SMTP host and sender of its own, although the
// panel's email settings already hold both. It may name only its recipients
// now, when the panel has a server for it to use, and is refused otherwise.
func TestAnEmailChannelMayUseThePanelsServer(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	create := func() (int, string) {
		t.Helper()
		r := post(t, admin, srv.URL+"/api/notifications/channels", map[string]any{
			"name": "ops mail", "type": "email", "enabled": true,
			"config": map[string]string{"to": "ops@example.test"},
		})
		var body struct{ Error string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		return r.StatusCode, body.Error
	}

	if code, msg := create(); code != http.StatusBadRequest || !strings.Contains(msg, "email settings") {
		t.Errorf("recipients only, and no server on the panel = %d %q, want 400 pointing at the email settings", code, msg)
	}
	if r := put(t, admin, srv.URL+"/api/email-settings", map[string]any{
		"host": "smtp.example.test", "port": "587", "from": "Quetzal <panel@example.test>", "tls": "starttls",
	}); r.StatusCode != http.StatusNoContent && r.StatusCode != http.StatusOK {
		t.Fatalf("email settings = %d", r.StatusCode)
	}
	if code, msg := create(); code != http.StatusCreated {
		t.Errorf("recipients only, with the panel's server = %d %q, want 201", code, msg)
	}
}
