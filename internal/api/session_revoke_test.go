package api_test

import (
	"net/http"
	"testing"
)

// Changing a password must end every other session: it is the first thing anyone
// does when they think a session was stolen, and it used to leave the stolen one
// working. Only the reset-by-email flow revoked anything.
func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})

	stolen := loginAs(t, srv.URL, "alice", "alicepw12")
	alice := loginAs(t, srv.URL, "alice", "alicepw12")
	if r, _ := stolen.Get(srv.URL + "/api/me"); r.StatusCode != http.StatusOK {
		t.Fatalf("precondition: second session = %d", r.StatusCode)
	}

	if r := post(t, alice, srv.URL+"/api/me/password", map[string]string{
		"oldPassword": "alicepw12", "newPassword": "brandnewpw99",
	}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("change password = %d", r.StatusCode)
	}
	if r, _ := stolen.Get(srv.URL + "/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("other session after the change = %d, want 401", r.StatusCode)
	}
	// The client that asked for the change stays signed in.
	if r, _ := alice.Get(srv.URL + "/api/me"); r.StatusCode != http.StatusOK {
		t.Errorf("own session after the change = %d, want 200", r.StatusCode)
	}
}

// An admin resetting a password is locking an account out; its live sessions
// must go with it. The admin's own session must survive.
func TestAdminPasswordResetEndsTargetSessions(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")

	var users []struct {
		ID       uint   `json:"id"`
		Username string `json:"username"`
	}
	getJSON(t, admin, srv.URL+"/api/users", &users)
	var aliceID uint
	for _, u := range users {
		if u.Username == "alice" {
			aliceID = u.ID
		}
	}
	if aliceID == 0 {
		t.Fatal("alice not found")
	}
	if rr := doPatch(t, admin, srv.URL+"/api/users/"+itoa(aliceID), map[string]any{
		"password": "adminresetpw1",
	}); rr.StatusCode != http.StatusOK {
		t.Fatalf("admin reset = %d", rr.StatusCode)
	}
	if r, _ := alice.Get(srv.URL + "/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("target session after an admin reset = %d, want 401", r.StatusCode)
	}
	if r, _ := admin.Get(srv.URL + "/api/me"); r.StatusCode != http.StatusOK {
		t.Errorf("admin's own session = %d, want 200", r.StatusCode)
	}
}

// An admin resetting their OWN password keeps the session they are working in.
func TestAdminResettingOwnPasswordKeepsCurrentSession(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	other := loginAs(t, srv.URL, "admin", "supersecret")

	var users []struct {
		ID       uint   `json:"id"`
		Username string `json:"username"`
	}
	getJSON(t, admin, srv.URL+"/api/users", &users)
	if rr := doPatch(t, admin, srv.URL+"/api/users/"+itoa(users[0].ID), map[string]any{
		"isAdmin": true, "password": "newadminpw123",
	}); rr.StatusCode != http.StatusOK {
		t.Fatalf("self reset = %d", rr.StatusCode)
	}
	if r, _ := admin.Get(srv.URL + "/api/me"); r.StatusCode != http.StatusOK {
		t.Errorf("own session = %d, want 200", r.StatusCode)
	}
	if r, _ := other.Get(srv.URL + "/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("admin's other session = %d, want 401", r.StatusCode)
	}
}
