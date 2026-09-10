package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A WebSocket upgrade from another origin cannot carry the session cookie
// anyway, but the check exists to be strict and localhost was a standing
// exception to it. It is now something a dev server asks for.
func TestWebSocketOriginRejectsLocalhostUnlessAsked(t *testing.T) {
	ts, admin, _, apiSrv, _ := newTestServerFull(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, ts.URL+"/api/servers", map[string]any{"name": "s", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	// The console refuses a stopped server before it ever looks at the origin, so
	// the check under test would never run.
	if pr := post(t, admin, ts.URL+"/api/servers/"+itoa(created.ID)+"/power", map[string]string{"action": "start"}); pr.StatusCode != http.StatusOK {
		t.Fatalf("start = %d", pr.StatusCode)
	}

	upgrade := func(origin string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/servers/"+itoa(created.ID)+"/console", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// gorilla answers a rejected origin with 403; anything else means the request
	// got past the check.
	if code := upgrade("http://localhost:5173"); code != http.StatusForbidden {
		t.Errorf("localhost origin with DevOrigin off = %d, want 403", code)
	}
	if code := upgrade("https://evil.example"); code != http.StatusForbidden {
		t.Errorf("a foreign origin = %d, want 403", code)
	}
	// Same-origin is what the panel itself sends, and must keep working.
	if code := upgrade(ts.URL); code == http.StatusForbidden {
		t.Error("same-origin upgrade was rejected")
	}
	// And a dev server can opt back in.
	apiSrv.DevOrigin = true
	if code := upgrade("http://localhost:5173"); code == http.StatusForbidden {
		t.Error("localhost origin still rejected with DevOrigin on")
	}
}
