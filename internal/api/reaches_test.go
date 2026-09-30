package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// A game server could not reach another inside the cluster, so a Velocity or
// BungeeCord proxy could only be joined to its servers through the internet,
// where they could then be joined directly. A server may now be given others
// to reach, as long as whoever does it may change them all, and they share a
// cluster.
func TestServerReachesOthersItWasGiven(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, ts.URL, map[string]any{"username": "bob", "password": "bobpw1234"})
	alice := loginAs(t, ts.URL, "alice", "alicepw12")
	bob := loginAs(t, ts.URL, "bob", "bobpw1234")

	create := func(c *http.Client, name string) models.Server {
		var s models.Server
		r := post(t, c, ts.URL+"/api/servers", map[string]any{"name": name, "template": "generic-process", "memory": "512Mi"})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create %s = %d", name, r.StatusCode)
		}
		json.NewDecoder(r.Body).Decode(&s)
		return s
	}
	proxy, lobby, survival := create(alice, "proxy"), create(alice, "lobby"), create(alice, "survival")
	bobs, lent := create(bob, "bobs"), create(bob, "lent")
	if rr := post(t, bob, ts.URL+"/api/servers/"+itoa(lent.ID)+"/access", map[string]any{"username": "alice", "permissions": []string{"view"}}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant = %d", rr.StatusCode)
	}
	reach := func(slugs ...string) (int, models.Server) {
		if slugs == nil {
			slugs = []string{} // null would leave the field as it is
		}
		var s models.Server
		r := doPatch(t, alice, ts.URL+"/api/servers/"+itoa(proxy.ID), map[string]any{"reaches": slugs})
		json.NewDecoder(r.Body).Decode(&s)
		return r.StatusCode, s
	}

	if code, s := reach(lobby.Slug, survival.Slug, lobby.Slug); code != http.StatusOK || len(s.Reaches) != 2 {
		t.Fatalf("reach her own servers = %d %v, want 200 and both, once", code, s.Reaches)
	}
	for name, c := range map[string]struct {
		slug string
		want int
	}{
		"itself":                        {proxy.Slug, http.StatusBadRequest},
		"a server she cannot see":       {bobs.Slug, http.StatusBadRequest},
		"a server she may only look at": {lent.Slug, http.StatusForbidden},
		"a server that does not exist":  {"nowhere-0000", http.StatusBadRequest},
	} {
		if code, _ := reach(c.slug); code != c.want {
			t.Errorf("reach %s = %d, want %d", name, code, c.want)
		}
	}
	// Servers of different clusters: a NetworkPolicy does not cross one.
	far, err := st.GetServer(survival.ID)
	if err != nil {
		t.Fatal(err)
	}
	far.ClusterID = 99
	if err := st.UpdateServer(far); err != nil {
		t.Fatal(err)
	}
	if code, _ := reach(survival.Slug); code != http.StatusBadRequest {
		t.Errorf("reach a server of another cluster = %d, want 400", code)
	}

	// What was refused left what she had.
	got, err := st.GetServer(proxy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reaches) != 2 || got.Reaches[0] != lobby.Slug {
		t.Errorf("reaches after the refusals = %v", got.Reaches)
	}
	if code, s := reach(); code != http.StatusOK || len(s.Reaches) != 0 {
		t.Errorf("reach nothing = %d %v", code, s.Reaches)
	}
}
