package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A server's name was set once, at creation, with no way to change it short of
// deleting the server. Renaming changes what is shown, never the slug the
// cluster objects and addresses hang off.
func TestRenameServer(t *testing.T) {
	ts, c := newTestServer(t)
	post(t, c, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	type server struct {
		ID          uint   `json:"id"`
		Slug        string `json:"slug"`
		DisplayName string `json:"displayName"`
	}
	var created server
	r := post(t, c, ts.URL+"/api/servers", map[string]any{"name": "  Survival  ", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	if created.DisplayName != "Survival" {
		t.Errorf("created name = %q, want it trimmed", created.DisplayName)
	}

	patch := func(body string) (*http.Response, server) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/servers/"+itoa(created.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		var out server
		json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	resp, renamed := patch(`{"name":" Creative (1.21) "}`)
	if resp.StatusCode != http.StatusOK || renamed.DisplayName != "Creative (1.21)" {
		t.Fatalf("rename = %d %q", resp.StatusCode, renamed.DisplayName)
	}
	if renamed.Slug != created.Slug {
		t.Errorf("slug moved from %q to %q", created.Slug, renamed.Slug)
	}
	var got server
	getJSON(t, c, ts.URL+"/api/servers/"+itoa(created.ID), &got)
	if got.DisplayName != "Creative (1.21)" {
		t.Errorf("stored name = %q", got.DisplayName)
	}

	for _, bad := range []string{`{"name":"   "}`, `{"name":"two\nlines"}`, `{"name":"` + strings.Repeat("é", 191) + `"}`} {
		if resp, _ := patch(bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%.40s: status %d, want 400", bad, resp.StatusCode)
		}
	}
	getJSON(t, c, ts.URL+"/api/servers/"+itoa(created.ID), &got)
	if got.DisplayName != "Creative (1.21)" {
		t.Errorf("a refused rename changed the name to %q", got.DisplayName)
	}

	if r := post(t, c, ts.URL+"/api/servers", map[string]any{"name": "a\tb", "template": "generic-process"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("creating with a control character = %d, want 400", r.StatusCode)
	}
}
