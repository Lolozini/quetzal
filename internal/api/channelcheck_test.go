package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A channel filtered on events the panel never records was accepted (201) and
// received nothing, without a word; one pointing at gopher:// or file:// was
// accepted and refused only when the first event went out. Both are refused
// when the channel is saved.
func TestAChannelIsCheckedWhenSaved(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	create := func(events []string, url string) (int, string) {
		t.Helper()
		r := post(t, admin, srv.URL+"/api/notifications/channels", map[string]any{
			"name": "ops", "type": "webhook", "enabled": true, "events": events,
			"config": map[string]string{"url": url},
		})
		var body struct {
			ID    uint
			Error string
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		return r.StatusCode, body.Error
	}

	if code, msg := create([]string{"backup.succeeded", "backup.succeded"}, "https://hooks.example.com/q"); code != http.StatusBadRequest || !strings.Contains(msg, `"backup.succeded"`) {
		t.Errorf("a misspelt event = %d %q, want 400 naming it", code, msg)
	}
	for _, bad := range []string{"gopher://hooks.example.com/q", "file:///etc/passwd", "hooks.example.com/q", "https://"} {
		if code, _ := create(nil, bad); code != http.StatusBadRequest {
			t.Errorf("url %q = %d, want 400", bad, code)
		}
	}
	if code, msg := create([]string{"backup.succeeded", "backup.failed", "server.stopped"}, "https://hooks.example.com/q"); code != http.StatusCreated {
		t.Fatalf("a good channel = %d %q", code, msg)
	}

	var types []string
	r, _ := admin.Get(srv.URL + "/api/notifications/event-types")
	_ = json.NewDecoder(r.Body).Decode(&types)
	if r.StatusCode != http.StatusOK || !strings.Contains(strings.Join(types, " "), "backup.succeeded") {
		t.Errorf("event types = %d %v", r.StatusCode, types)
	}
}
