package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A sender written "Name <address>" was saved, and then every mail failed at
// the relay, password resets included, which nobody notices until someone
// needs one. That form is now sent properly; what is not an address at all is
// refused when it is saved, in the panel's settings and in an email channel.
func TestAnEmailSenderIsCheckedWhenSaved(t *testing.T) {
	srv, admin, _ := newMailHarness(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	set := func(from string) *http.Response {
		return doPut(t, admin, srv.URL+"/api/email-settings", map[string]string{"host": "smtp.example", "from": from, "publicUrl": "https://panel.example"})
	}
	if r := set("Quetzal <quetzal@example.com>"); r.StatusCode != http.StatusNoContent {
		t.Errorf("a sender with a name = %d, want 204", r.StatusCode)
	}
	for _, bad := range []string{"Quetzal", "quetzal@", ""} {
		r := set(bad)
		body, _ := io.ReadAll(r.Body)
		if r.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "email address") {
			t.Errorf("sender %q = %d %s, want 400 saying what is expected", bad, r.StatusCode, body)
		}
	}
	var got map[string]any
	getJSON(t, admin, srv.URL+"/api/email-settings", &got)
	if got["from"] != "Quetzal <quetzal@example.com>" {
		t.Errorf("stored sender %v, want the last accepted one", got["from"])
	}

	channel := func(from string) *http.Response {
		return post(t, admin, srv.URL+"/api/notifications/channels", map[string]any{
			"name": "ops", "type": "email", "enabled": true, "serverId": 0,
			"config": map[string]string{"host": "smtp.example", "from": from, "to": "ops@example.com"},
		})
	}
	if r := channel("Quetzal"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a channel sending as %q = %d, want 400", "Quetzal", r.StatusCode)
	}
	r := channel("Quetzal <quetzal@example.com>")
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("a channel with a named sender = %d, want 201", r.StatusCode)
	}
	var ch struct{ ID uint }
	_ = json.NewDecoder(r.Body).Decode(&ch)
	if r := doPatch(t, admin, srv.URL+"/api/notifications/channels/"+itoa(ch.ID), map[string]any{
		"config": map[string]string{"from": "nobody"},
	}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("changing the channel's sender to %q = %d, want 400", "nobody", r.StatusCode)
	}
}
