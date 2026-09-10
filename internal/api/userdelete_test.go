package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func deleteAs(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// Deleting an account used to leave its servers running with an owner id
// pointing at nothing: nobody to answer for them, and the owner-based resource
// quota silently skipped on every later edit. They are handed to the admin who
// does the deleting instead.
func TestDeletingAnOwnerReassignsTheirServers(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")

	var created struct{ ID uint }
	r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "orphan", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)

	adminID := userID(t, admin, srv.URL, "admin")
	if code := deleteAs(t, admin, srv.URL+"/api/users/"+itoa(userID(t, admin, srv.URL, "alice"))); code != http.StatusNoContent {
		t.Fatalf("delete alice = %d", code)
	}

	var got struct {
		ID      uint `json:"id"`
		OwnerID uint `json:"ownerId"`
	}
	getJSON(t, admin, srv.URL+"/api/servers/"+itoa(created.ID), &got)
	if got.OwnerID != adminID {
		t.Errorf("ownerId = %d, want the deleting admin %d", got.OwnerID, adminID)
	}
}

// A scoped users-admin may manage accounts, but handing themselves a server they
// had no rights on is a different power. They have to leave that to someone who
// administers servers.
func TestScopedUsersAdminCannotInheritServersByDeletingTheOwner(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	roleID := createRole(t, admin, srv.URL, "user-manager", []string{"users"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, srv.URL, map[string]any{"username": "mel", "password": "melpw12345"})
	if rr := setUserRole(t, admin, srv.URL, userID(t, admin, srv.URL, "mel"), &roleID); rr.StatusCode != http.StatusOK {
		t.Fatalf("assign role = %d", rr.StatusCode)
	}

	alice := loginAs(t, srv.URL, "alice", "alicepw12")
	var created struct{ ID uint }
	json.NewDecoder(post(t, alice, srv.URL+"/api/servers",
		map[string]any{"name": "coveted", "template": "generic-process"}).Body).Decode(&created)

	mel := loginAs(t, srv.URL, "mel", "melpw12345")
	aliceID := userID(t, admin, srv.URL, "alice")
	if code := deleteAs(t, mel, srv.URL+"/api/users/"+itoa(aliceID)); code != http.StatusConflict {
		t.Errorf("scoped users-admin deleting a server owner = %d, want 409", code)
	}
	// The account and its server are still there.
	var got struct {
		OwnerID uint `json:"ownerId"`
	}
	getJSON(t, admin, srv.URL+"/api/servers/"+itoa(created.ID), &got)
	if got.OwnerID != aliceID {
		t.Errorf("ownerId = %d, want alice %d", got.OwnerID, aliceID)
	}
	// A user who owns nothing is still theirs to delete.
	createUser(t, admin, srv.URL, map[string]any{"username": "bob", "password": "bobpw12345"})
	if code := deleteAs(t, mel, srv.URL+"/api/users/"+itoa(userID(t, admin, srv.URL, "bob"))); code != http.StatusNoContent {
		t.Errorf("deleting a user who owns nothing = %d, want 204", code)
	}
}
