package api_test

import (
	"bytes"
	"encoding/json"
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

// A storage size reaches resource.MustParse in the builders, which panics on
// anything it cannot read — inside the controller's reconcile loop, which has no
// recover. The row survives the crash, so the controller reads it again on
// restart and dies again: reconciliation stops for every server on the cluster
// until someone edits the database by hand. "10 GB" instead of "10Gi" is enough,
// and any account that can create a server can do it.
func TestStorageSizeIsValidatedBeforeItReachesTheBuilders(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	create := func(size string) int {
		t.Helper()
		return post(t, admin, srv.URL+"/api/servers", map[string]any{
			"name": "s", "template": "generic-process",
			"storage": map[string]any{"type": "pvc", "size": size},
		}).StatusCode
	}
	for _, bad := range []string{"10 GB", "not-a-size", "-5Gi", "0", "1TB", "٤Gi"} {
		if code := create(bad); code != http.StatusBadRequest {
			t.Errorf("storage size %q accepted: %d", bad, code)
		}
	}
	for _, good := range []string{"10Gi", "500Mi", "1T", ""} {
		if code := create(good); code != http.StatusCreated {
			t.Errorf("storage size %q refused: %d", good, code)
		}
	}
}

// Same reasoning for a managed database host: its size lands in the same
// MustParse, in the same loop.
func TestManagedDatabaseStorageSizeIsValidated(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	create := func(size string) int {
		t.Helper()
		return post(t, admin, srv.URL+"/api/database-hosts", map[string]any{
			"name": "db" + size, "kind": "managed", "clusterId": 1, "storageSize": size,
		}).StatusCode
	}
	for _, bad := range []string{"10 GB", "lots", "-1Gi"} {
		if code := create(bad); code != http.StatusBadRequest {
			t.Errorf("storage size %q accepted: %d", bad, code)
		}
	}
	if code := create("5Gi"); code != http.StatusCreated {
		t.Errorf("a good size was refused: %d", code)
	}
}

// Service annotations are copied onto the Service verbatim and other controllers
// act on them: external-dns creates the record a hostname annotation names,
// MetalLB hands out the address one asks for, a cloud provider bills for the
// load balancer it is told to make. On a shared install that let a player claim
// a record in the operator's own DNS zone. The panel never offered it; the
// endpoint did.
func TestServiceAnnotationsAreAdminOnly(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "player", "password": "playerpw12"})
	player := loginAs(t, srv.URL, "player", "playerpw12")

	hijack := map[string]string{"external-dns.alpha.kubernetes.io/hostname": "www.operator.example"}
	body := func() map[string]any {
		return map[string]any{
			"name": "s", "template": "generic-process",
			"ports":  []map[string]any{{"port": 25565, "protocol": "TCP"}},
			"expose": map[string]any{"type": "NodePort", "annotations": hijack},
		}
	}
	if r := post(t, player, srv.URL+"/api/servers", body()); r.StatusCode != http.StatusForbidden {
		t.Errorf("a player set a service annotation: %d", r.StatusCode)
	}
	r := post(t, admin, srv.URL+"/api/servers", body())
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("an admin could not set one: %d", r.StatusCode)
	}
	var created struct{ ID uint }
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()

	// A subuser with the settings permission reaches the update path legitimately;
	// that is who this has to hold against.
	post(t, admin, srv.URL+"/api/servers/"+itoa(created.ID)+"/access", map[string]any{
		"username": "player", "permissions": []string{"view", "settings"},
	})

	// Editing the exposure as a non-admin keeps what the admin set rather than
	// dropping it, and still cannot introduce one.
	patch := func(c *http.Client, ann map[string]string) (int, map[string]any) {
		t.Helper()
		exp := map[string]any{"type": "NodePort"}
		if ann != nil {
			exp["annotations"] = ann
		}
		b, _ := json.Marshal(map[string]any{"expose": exp})
		req, _ := http.NewRequest(http.MethodPatch,
			srv.URL+"/api/servers/"+itoa(created.ID), bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("PATCH: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := patch(admin, nil); code != http.StatusOK {
		t.Fatalf("admin patch = %d", code)
	}
	if code, _ := patch(player, map[string]string{"external-dns.alpha.kubernetes.io/hostname": "evil.example"}); code != http.StatusForbidden {
		t.Errorf("a player replaced the annotations: %d", code)
	}
}
