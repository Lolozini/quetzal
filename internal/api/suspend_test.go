package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Suspending a server refused its owner power and files, and nothing else: the
// owner of a server suspended for abuse could still delete it, data and all,
// before anyone looked into it, rename it, or queue backup after backup until
// retention had pushed out every snapshot from before. Suspended, a server is
// now frozen for its owner and subusers, who may look at it and nothing more,
// until an administrator lifts the suspension.
func TestSuspendedServerIsFrozenForItsOwner(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, srv.URL, map[string]any{"username": "bob", "password": "bobpw1234"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")
	bob := loginAs(t, srv.URL, "bob", "bobpw1234")

	var created struct{ ID uint }
	r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "evidence", "template": "generic-process", "memory": "512Mi"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	url := srv.URL + "/api/servers/" + itoa(created.ID)
	all := []string{"view", "power", "console", "schedules", "backups", "files", "settings", "databases", "delete"}
	if rr := post(t, alice, url+"/access", map[string]any{"username": "bob", "permissions": all}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant bob = %d", rr.StatusCode)
	}
	if rr := post(t, admin, url+"/suspend", nil); rr.StatusCode != http.StatusOK {
		t.Fatalf("suspend = %d", rr.StatusCode)
	}

	// What they may still do: look.
	for _, path := range []string{"", "/schedules", "/backups", "/events", "/audit"} {
		for name, c := range map[string]*http.Client{"owner": alice, "subuser": bob} {
			rr, err := c.Get(url + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			if rr.StatusCode != http.StatusOK {
				t.Errorf("%s GET %s while suspended = %d, want 200", name, path, rr.StatusCode)
			}
		}
	}

	// What they may not. Each would otherwise have gone through (or failed
	// for want of a backup target, a database host, a cluster).
	refused := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"rename", http.MethodPatch, "", map[string]any{"name": "renamed"}},
		{"reinstall", http.MethodPost, "/reinstall", map[string]any{}},
		{"back up", http.MethodPost, "/backups", nil},
		{"add a schedule", http.MethodPost, "/schedules", map[string]any{"name": "x", "cron": "0 4 * * *", "action": "backup", "enabled": true}},
		{"create a database", http.MethodPost, "/databases", map[string]any{"hostId": 1, "name": "db"}},
		{"list its databases", http.MethodGet, "/databases", nil},
		{"list files", http.MethodGet, "/files?path=", nil},
		{"read the setup log", http.MethodGet, "/install-log", nil},
		{"list its notification channels", http.MethodGet, "/notifications", nil},
		{"power", http.MethodPost, "/power", map[string]string{"action": "start"}},
		{"delete", http.MethodDelete, "", nil}, // last: it would take the rest with it
	}
	for _, c := range refused {
		for name, client := range map[string]*http.Client{"owner": alice, "subuser": bob} {
			rr := doMethod(t, client, c.method, url+c.path, c.body)
			if rr.StatusCode != http.StatusConflict {
				t.Errorf("%s may %s while suspended: %d, want 409", name, c.name, rr.StatusCode)
			}
		}
	}
	if rr := post(t, alice, url+"/access", map[string]any{"username": "admin", "permissions": []string{"view"}}); rr.StatusCode != http.StatusConflict {
		t.Errorf("owner may grant access while suspended: %d, want 409", rr.StatusCode)
	}
	if rr := doMethod(t, alice, http.MethodDelete, url+"/access/3", nil); rr.StatusCode != http.StatusConflict {
		t.Errorf("owner may revoke access while suspended: %d, want 409", rr.StatusCode)
	}
	if rr := post(t, alice, srv.URL+"/api/notifications/channels", map[string]any{
		"name": "hook", "type": "webhook", "enabled": true, "serverId": created.ID,
		"config": map[string]string{"url": "https://example.com/hook"},
	}); rr.StatusCode != http.StatusConflict {
		t.Errorf("owner may add a notification channel while suspended: %d, want 409", rr.StatusCode)
	}
	if rr, err := alice.Get(url); err != nil || rr.StatusCode != http.StatusOK {
		t.Fatalf("the server is gone after the owner's attempts: %v %v", rr, err)
	}

	// The administrator who suspended it keeps every right, and lifting the
	// suspension gives the owner theirs back.
	if rr := doPatch(t, admin, url, map[string]any{"name": "looked into"}); rr.StatusCode != http.StatusOK {
		t.Errorf("admin rename while suspended = %d, want 200", rr.StatusCode)
	}
	if rr := post(t, admin, url+"/unsuspend", nil); rr.StatusCode != http.StatusOK {
		t.Fatalf("unsuspend = %d", rr.StatusCode)
	}
	if rr := doPatch(t, alice, url, map[string]any{"name": "mine again"}); rr.StatusCode != http.StatusOK {
		t.Errorf("owner rename after the suspension = %d, want 200", rr.StatusCode)
	}
}
