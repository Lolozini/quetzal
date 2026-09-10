package api_test

import (
	"net/http"
	"testing"
)

// The namespace of a managed host is Quetzal's to choose. It creates that
// namespace and deletes it when the host goes, so a name pointing at one that
// already exists would put a workload in someone else's namespace and then
// collect it. The endpoint is reachable by an admin scoped to database hosts
// alone, which makes this an escalation out of a delegated role.
func TestManagedDatabaseHostCannotClaimAForeignNamespace(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	create := func(ns string) *http.Response {
		t.Helper()
		return post(t, admin, srv.URL+"/api/database-hosts", map[string]any{
			"name": "db-" + ns, "kind": "managed", "clusterId": 1,
			"namespace": ns, "storageSize": "1Gi",
		})
	}
	for _, ns := range []string{"kube-system", "default", "quetzal", "quetzal-srv-victim", "../kube-system"} {
		if r := create(ns); r.StatusCode != http.StatusBadRequest {
			t.Errorf("namespace %q accepted: %d", ns, r.StatusCode)
		}
	}
	// A name Quetzal could have chosen itself is still the operator's to pick,
	// and so is leaving it out.
	for _, ns := range []string{"quetzal-db-analytics", ""} {
		if r := create(ns); r.StatusCode != http.StatusCreated {
			t.Errorf("namespace %q refused: %d", ns, r.StatusCode)
		}
	}
}

// A template's image list is the operator's curation, and the panel presents it
// as a choice among those. Taking whatever the request names let any account
// with a quota run any container instead — confined by the pod's own limits,
// but not what the operator put on the menu.
func TestServerImageMustBeOneTheTemplateOffers(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "player", "password": "playerpw12"})
	player := loginAs(t, srv.URL, "player", "playerpw12")

	create := func(c *http.Client, image string) int {
		t.Helper()
		body := map[string]any{"name": "s", "template": "generic-process"}
		if image != "" {
			body["image"] = image
		}
		return post(t, c, srv.URL+"/api/servers", body).StatusCode
	}
	if code := create(player, "docker.io/evil/miner:latest"); code != http.StatusBadRequest {
		t.Errorf("an image outside the template was accepted: %d", code)
	}
	if code := create(player, ""); code != http.StatusCreated {
		t.Errorf("the template's default image was refused: %d", code)
	}
	// An admin may still pin something else — that is how a new tag gets tried
	// before it goes on the menu.
	if code := create(admin, "docker.io/library/alpine:3.20"); code != http.StatusCreated {
		t.Errorf("an admin could not pin an image: %d", code)
	}
}
