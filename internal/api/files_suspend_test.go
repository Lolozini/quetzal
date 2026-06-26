package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestSuspendedServerBlocksOfflineFilesForOwner verifies that suspension (an
// admin-enforced freeze) also blocks file management for the owner — otherwise
// the data-manager pod (which is always up) would let an owner edit files of a
// server an admin deliberately suspended. The 403 is returned before any pod is
// touched, so this exercises the gate without needing a cluster.
func TestSuspendedServerBlocksOfflineFilesForOwner(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")

	var created struct{ ID uint }
	r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "alice srv", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	files := srv.URL + "/api/servers/" + itoa(created.ID) + "/files?path="

	// Admin suspends the (stopped) server.
	if rr := post(t, admin, srv.URL+"/api/servers/"+itoa(created.ID)+"/suspend", nil); rr.StatusCode != http.StatusOK {
		t.Fatalf("suspend = %d, want 200", rr.StatusCode)
	}

	// Owner can no longer list files: suspension freezes file access too.
	rr, err := alice.Get(files)
	if err != nil {
		t.Fatalf("GET files: %v", err)
	}
	if rr.StatusCode != http.StatusForbidden {
		t.Fatalf("owner files on suspended server = %d, want 403", rr.StatusCode)
	}
}
