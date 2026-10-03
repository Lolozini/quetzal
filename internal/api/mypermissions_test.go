package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// The panel drew a server's page for its owner whoever opened it: a subuser
// given the files had no Files tab, and one without power saw power buttons
// that answered 403. A server now comes with what the reader may do on it.
func TestAServerSaysWhatItsReaderMayDo(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, srv.URL, map[string]any{"username": "bob", "password": "bobpw1234"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")
	bob := loginAs(t, srv.URL, "bob", "bobpw1234")

	var created struct {
		ID            uint
		MyPermissions []string
	}
	r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "alice srv", "template": "generic-process", "memory": "512Mi"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	if !slices.Equal(created.MyPermissions, models.AllPermissions) {
		t.Errorf("the creator holds %v, want every permission", created.MyPermissions)
	}
	url := srv.URL + "/api/servers/" + itoa(created.ID)
	if rr := post(t, alice, url+"/access", map[string]any{"username": "bob", "permissions": []string{"view", "console", "files"}}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant = %d", rr.StatusCode)
	}

	perms := func(c *http.Client) []string {
		t.Helper()
		var got struct{ MyPermissions []string }
		getJSON(t, c, url, &got)
		return got.MyPermissions
	}
	if got := perms(bob); !slices.Equal(got, []string{"view", "console", "files"}) {
		t.Errorf("the subuser holds %v, want view, console, files", got)
	}
	if got := perms(alice); !slices.Equal(got, models.AllPermissions) {
		t.Errorf("the owner holds %v", got)
	}
	if got := perms(admin); !slices.Equal(got, models.AllPermissions) {
		t.Errorf("an administrator holds %v", got)
	}
}
