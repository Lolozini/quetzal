package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The recette of 0.10.0 took a server from NodePort to ClusterIP and back:
// its port went from 30003 to 30027, and the old one back to the pool, free for
// the next server (R-27). A server keeps its node ports while it exists.
func TestAServerKeepsItsNodePortThroughAnotherExposure(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{
		"name": "s", "template": "generic-process", "ports": []map[string]any{{"port": 25565, "protocol": "TCP", "primary": true}},
		"expose": map[string]string{"type": "NodePort"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	url := srv.URL + "/api/servers/" + itoa(created.ID)
	nodePort := func() int {
		t.Helper()
		var s struct {
			Ports []struct{ NodePort int }
		}
		getJSON(t, admin, url, &s)
		if len(s.Ports) == 0 {
			return 0
		}
		return s.Ports[0].NodePort
	}
	expose := func(kind string) {
		t.Helper()
		resp := doPatch(t, admin, url, map[string]any{"expose": map[string]string{"type": kind}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expose %s = %d", kind, resp.StatusCode)
		}
	}
	first := nodePort()
	if first == 0 {
		t.Fatal("no node port at creation")
	}
	expose("ClusterIP")
	expose("NodePort")
	if again := nodePort(); again != first {
		t.Errorf("node port %d after ClusterIP and back, want %d", again, first)
	}
}
