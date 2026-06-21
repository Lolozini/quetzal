package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// loginAs returns a cookie-jar client authenticated as the given user.
func loginAs(t *testing.T, ts string, username, password string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if r := post(t, c, ts+"/api/login", map[string]string{"username": username, "password": password}); r.StatusCode != http.StatusOK {
		t.Fatalf("login %s = %d", username, r.StatusCode)
	}
	return c
}

func createUser(t *testing.T, admin *http.Client, ts string, body map[string]any) {
	t.Helper()
	if r := post(t, admin, ts+"/api/users", body); r.StatusCode != http.StatusCreated {
		t.Fatalf("create user = %d", r.StatusCode)
	}
}

func TestMultiTenantOwnershipAndSubusers(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, srv.URL, map[string]any{"username": "bob", "password": "bobpw1234"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")
	bob := loginAs(t, srv.URL, "bob", "bobpw1234")

	// Alice creates a server (she owns it).
	var created struct{ ID uint }
	r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "alice srv", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("alice create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	url := srv.URL + "/api/servers/" + itoa(created.ID)

	// Bob can't see it (not owner, not subuser).
	var bobList []map[string]any
	getJSON(t, bob, srv.URL+"/api/servers", &bobList)
	if len(bobList) != 0 {
		t.Errorf("bob should see 0 servers, saw %d", len(bobList))
	}
	if rr, _ := bob.Get(url); rr.StatusCode != http.StatusNotFound {
		t.Errorf("bob get alice server = %d, want 404", rr.StatusCode)
	}

	// Admin sees all.
	var adminList []map[string]any
	getJSON(t, admin, srv.URL+"/api/servers", &adminList)
	if len(adminList) != 1 {
		t.Errorf("admin should see 1 server, saw %d", len(adminList))
	}

	// Alice grants bob view+power (not delete).
	if rr := post(t, alice, url+"/access", map[string]any{"username": "bob", "permissions": []string{"view", "power"}}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant = %d", rr.StatusCode)
	}

	// Bob now sees the shared server in his list and can open it.
	getJSON(t, bob, srv.URL+"/api/servers", &bobList)
	if len(bobList) != 1 {
		t.Errorf("bob should see 1 shared server after grant, saw %d", len(bobList))
	}
	if rr, _ := bob.Get(url); rr.StatusCode != http.StatusOK {
		t.Errorf("bob get after grant = %d, want 200", rr.StatusCode)
	}
	if rr := post(t, bob, url+"/power", map[string]string{"action": "start"}); rr.StatusCode != http.StatusOK {
		t.Errorf("bob power = %d, want 200", rr.StatusCode)
	}
	if rr := post(t, bob, url+"/schedules", map[string]any{"name": "x", "cron": "* * * * *", "action": "restart", "enabled": true}); rr.StatusCode != http.StatusForbidden {
		t.Errorf("bob schedule = %d, want 403", rr.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if dr, _ := bob.Do(req); dr.StatusCode != http.StatusForbidden {
		t.Errorf("bob delete = %d, want 403", dr.StatusCode)
	}

	// Bob can't manage access (only owner/admin).
	if rr := post(t, bob, url+"/access", map[string]any{"username": "alice", "permissions": []string{"view"}}); rr.StatusCode != http.StatusForbidden {
		t.Errorf("bob grant = %d, want 403", rr.StatusCode)
	}
}

func TestSuspendBlocksPower(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")

	var created struct{ ID uint }
	r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "s", "template": "generic-process", "start": true})
	json.NewDecoder(r.Body).Decode(&created)
	url := srv.URL + "/api/servers/" + itoa(created.ID)

	// Alice cannot suspend (admin only).
	if rr := post(t, alice, url+"/suspend", nil); rr.StatusCode != http.StatusForbidden {
		t.Errorf("alice suspend = %d, want 403", rr.StatusCode)
	}
	// Admin suspends; alice can no longer power it.
	if rr := post(t, admin, url+"/suspend", nil); rr.StatusCode != http.StatusOK {
		t.Fatalf("admin suspend = %d", rr.StatusCode)
	}
	if rr := post(t, alice, url+"/power", map[string]string{"action": "start"}); rr.StatusCode != http.StatusConflict {
		t.Errorf("power while suspended = %d, want 409", rr.StatusCode)
	}
	// Admin unsuspends; power works again.
	if rr := post(t, admin, url+"/unsuspend", nil); rr.StatusCode != http.StatusOK {
		t.Fatalf("unsuspend = %d", rr.StatusCode)
	}
	if rr := post(t, alice, url+"/power", map[string]string{"action": "stop"}); rr.StatusCode != http.StatusOK {
		t.Errorf("power after unsuspend = %d, want 200", rr.StatusCode)
	}
}

func TestRestoreRequiresStoppedServer(t *testing.T) {
	srv, admin, st := newTestServerStore(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	var created struct{ ID uint }
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "s", "template": "generic-process", "start": true})
	json.NewDecoder(r.Body).Decode(&created)
	url := srv.URL + "/api/servers/" + itoa(created.ID)

	// Seed a succeeded backup to restore from (the manager isn't running here).
	b := &models.Backup{ServerID: created.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded}
	if err := st.CreateBackup(b); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	restoreURL := url + "/backups/" + itoa(b.ID) + "/restore"

	// Running server: a live restore would corrupt the volume -> 409.
	if rr := post(t, admin, restoreURL, nil); rr.StatusCode != http.StatusConflict {
		t.Errorf("restore while running = %d, want 409", rr.StatusCode)
	}
	// Stop it, then restore is accepted.
	if rr := post(t, admin, url+"/power", map[string]string{"action": "stop"}); rr.StatusCode != http.StatusOK {
		t.Fatalf("stop = %d", rr.StatusCode)
	}
	if rr := post(t, admin, restoreURL, nil); rr.StatusCode != http.StatusAccepted {
		t.Errorf("restore while stopped = %d, want 202", rr.StatusCode)
	}
}

func TestQuotaEnforcement(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12", "maxServers": 1})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")

	if r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "one", "template": "generic-process"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d", r.StatusCode)
	}
	if r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "two", "template": "generic-process"}); r.StatusCode != http.StatusForbidden {
		t.Errorf("second create = %d, want 403 (quota)", r.StatusCode)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	var resp struct {
		Token string `json:"token"`
	}
	r := post(t, admin, srv.URL+"/api/apikeys", map[string]string{"name": "ci"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create key = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&resp)
	if !strings.HasPrefix(resp.Token, "qk_") {
		t.Fatalf("token = %q", resp.Token)
	}

	// Use the API key as a Bearer token (no cookie).
	noCookie := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	got, err := noCookie.Do(req)
	if err != nil || got.StatusCode != http.StatusOK {
		t.Fatalf("apikey /me = %v / %d", err, got.StatusCode)
	}
	// A bogus key is rejected.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req2.Header.Set("Authorization", "Bearer qk_deadbeef")
	if g2, _ := noCookie.Do(req2); g2.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus key = %d, want 401", g2.StatusCode)
	}
}

func TestLastAdminProtected(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// Find admin's own id via /api/users.
	var users []struct {
		ID      uint `json:"id"`
		IsAdmin bool `json:"isAdmin"`
	}
	getJSON(t, admin, srv.URL+"/api/users", &users)
	if len(users) != 1 {
		t.Fatalf("users = %d", len(users))
	}
	url := srv.URL + "/api/users/" + itoa(users[0].ID)

	// Can't delete self / last admin.
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if dr, _ := admin.Do(req); dr.StatusCode != http.StatusConflict {
		t.Errorf("delete self = %d, want 409", dr.StatusCode)
	}
	// Can't demote the last admin.
	preq, _ := http.NewRequest(http.MethodPatch, url, strings.NewReader(`{"isAdmin":false}`))
	preq.Header.Set("Content-Type", "application/json")
	if pr, _ := admin.Do(preq); pr.StatusCode != http.StatusConflict {
		t.Errorf("demote last admin = %d, want 409", pr.StatusCode)
	}
}
