package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// The published host goes in front of ":<port>" in every server's address, and
// anything was taken: with "bad host!" saved (204), players were handed
// "bad host!:30158". It is a DNS name or an IP address now, panel-wide and per
// cluster alike.
func TestThePublishedHostIsAHostPlayersCanReach(t *testing.T) {
	ts, c, st := newTestServerStore(t)
	setupAdmin(t, ts.URL, c)
	for _, bad := range []string{"bad host!", "https://play.example.com", "play.example.com:25565", "play..example.com", "-play.example.com", "play_example.com"} {
		if r := put(t, c, ts.URL+"/api/network-settings", map[string]string{"endpointHost": bad}); r.StatusCode != http.StatusBadRequest {
			t.Errorf("endpointHost %q = %d, want 400", bad, r.StatusCode)
		}
	}
	if v, _ := st.GetSetting(store.SettingEndpointHost); v != "" {
		t.Errorf("a refused host was saved: %q", v)
	}
	for _, good := range []string{"play.example.com", "Play.Example.com.", "mc", "192.168.1.13", "fd00::1", ""} {
		if r := put(t, c, ts.URL+"/api/network-settings", map[string]string{"endpointHost": good}); r.StatusCode != http.StatusNoContent {
			t.Errorf("endpointHost %q = %d, want 204", good, r.StatusCode)
		}
	}

	if r := post(t, c, ts.URL+"/api/clusters", map[string]string{"name": "edge", "kubeconfig": fakeKubeconfig, "endpointHost": "bad host!"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("new cluster with endpointHost %q = %d, want 400", "bad host!", r.StatusCode)
	}
	var edge struct{ ID uint }
	r := post(t, c, ts.URL+"/api/clusters", map[string]string{"name": "edge", "kubeconfig": fakeKubeconfig, "endpointHost": "edge.example.com"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("new cluster = %d", r.StatusCode)
	}
	_ = json.NewDecoder(r.Body).Decode(&edge)
	if r := doMethod(t, c, http.MethodPatch, ts.URL+"/api/clusters/"+itoa(edge.ID), map[string]string{"endpointHost": "bad host!"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("cluster endpointHost %q = %d, want 400", "bad host!", r.StatusCode)
	}
	if got, _ := st.GetCluster(edge.ID); got.EndpointHost != "edge.example.com" {
		t.Errorf("a refused cluster host was saved: %q", got.EndpointHost)
	}
}

// Deleting an account hands its servers to whoever deletes it, which the
// confirmation did not say ("their servers are NOT deleted"). The user list
// counts each account's servers so that it can.
func TestTheUserListCountsTheirServers(t *testing.T) {
	ts, c, st := newTestServerStore(t)
	setupAdmin(t, ts.URL, c)
	createUser(t, c, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice, err := st.GetUserByUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"a-1", "a-2"} {
		if err := st.CreateServer(&models.Server{Slug: slug, Namespace: "qz-" + slug, OwnerID: alice.ID}); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := c.Get(ts.URL + "/api/users")
	var users []struct {
		Username string
		Servers  int
	}
	_ = json.NewDecoder(r.Body).Decode(&users)
	got := []string{}
	for _, u := range users {
		got = append(got, u.Username+"="+itoa(uint(u.Servers)))
	}
	if strings.Join(got, " ") != "admin=0 alice=2" {
		t.Errorf("users and their servers: %v", got)
	}
}
