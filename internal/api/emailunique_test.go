package api_test

import (
	"net/http"
	"testing"
)

// The recette of 0.10.0 gave qa-user1 the address of qa-admin, in another
// case, and was answered 200 (R-13): a reset by email then went to the oldest
// of the two accounts. An address is one account's now.
func TestAnEmailAddressIsOneAccounts(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret", "email": "admin@example.com"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")

	if r := put(t, alice, srv.URL+"/api/me/email", map[string]string{"email": "Admin@Example.com"}); r.StatusCode != http.StatusConflict {
		t.Errorf("taking another account's address = %d, want 409", r.StatusCode)
	}
	if r := post(t, admin, srv.URL+"/api/users", map[string]any{"username": "bob", "password": "bobpw1234", "email": "ADMIN@example.com"}); r.StatusCode != http.StatusConflict {
		t.Errorf("creating an account on a taken address = %d, want 409", r.StatusCode)
	}
	if r := put(t, alice, srv.URL+"/api/me/email", map[string]string{"email": "alice@example.com"}); r.StatusCode != http.StatusOK {
		t.Errorf("a free address = %d", r.StatusCode)
	}
	// Setting one's own address again, in another case, is no conflict.
	if r := put(t, admin, srv.URL+"/api/me/email", map[string]string{"email": "admin@EXAMPLE.com"}); r.StatusCode != http.StatusOK {
		t.Errorf("one's own address = %d", r.StatusCode)
	}
}
