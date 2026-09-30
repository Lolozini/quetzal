package api_test

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// A database host read "unreachable, never checked" until an administrator
// pressed "test". An external one is checked as it is added: here a port
// nothing listens on, which it says.
func TestAnExternalDatabaseHostIsCheckedWhenAdded(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // a port that refuses connections

	r := post(t, admin, srv.URL+"/api/database-hosts", map[string]any{
		"name": "lan", "kind": "external", "host": "127.0.0.1", "port": port,
		"adminUser": "root", "adminPassword": "pw",
	})
	var h struct {
		Reachable     bool
		StatusMessage string
		LastCheckedAt *time.Time
	}
	_ = json.NewDecoder(r.Body).Decode(&h)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	if h.LastCheckedAt == nil || h.Reachable || h.StatusMessage == "" {
		t.Errorf("a host added on 127.0.0.1:%s, where nothing listens: checked at %v, reachable %v, %q -- want it checked and said unreachable", strconv.Itoa(port), h.LastCheckedAt, h.Reachable, h.StatusMessage)
	}
}
