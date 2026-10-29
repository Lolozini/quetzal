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
		body := map[string]any{"name": "s", "template": "generic-process", "memory": "512Mi"}
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
	if code := create(admin, "docker.io/library/alpine:3.24"); code != http.StatusCreated {
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

// Switching the exposure type in the panel sends the type alone. Taking that as
// the whole exposure dropped the annotations an admin had set, and external-dns,
// in sync mode, deleted the server's DNS record; a preserveClientIP turned off
// came back on.
func TestExposePatchKeepsWhatItLeavesOut(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	const hostname = "external-dns.alpha.kubernetes.io/hostname"
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{
		"name": "s", "template": "generic-process",
		"ports": []map[string]any{{"port": 25565, "protocol": "TCP"}},
		"expose": map[string]any{
			"type": "NodePort", "annotations": map[string]string{hostname: "mc.example.org"},
			"preserveClientIP": false,
		},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	var created struct{ ID uint }
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	url := srv.URL + "/api/servers/" + itoa(created.ID)

	type exposure struct {
		Type             string
		Annotations      map[string]string
		PreserveClientIP *bool
	}
	patch := func(body map[string]any) exposure {
		t.Helper()
		resp := doPatch(t, admin, url, map[string]any{"expose": body})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH %v = %d", body, resp.StatusCode)
		}
		var got struct{ Expose exposure }
		getJSON(t, admin, url, &got)
		return got.Expose
	}

	got := patch(map[string]any{"type": "ClusterIP"})
	if got.Type != "ClusterIP" {
		t.Errorf("type = %q, want ClusterIP", got.Type)
	}
	if got.Annotations[hostname] != "mc.example.org" {
		t.Errorf("switching the type dropped the annotations: %v", got.Annotations)
	}
	if got.PreserveClientIP == nil || *got.PreserveClientIP {
		t.Errorf("switching the type reset preserveClientIP: %v", got.PreserveClientIP)
	}

	got = patch(map[string]any{"annotations": map[string]string{}})
	if len(got.Annotations) != 0 {
		t.Errorf("an empty annotations object left %v", got.Annotations)
	}
	if got.Type != "ClusterIP" {
		t.Errorf("clearing the annotations changed the type to %q", got.Type)
	}
}

// A port name goes on both the Service and the container port, and Kubernetes
// refuses what it does not like. Accepting one it will refuse created the server
// happily and then had its Service rejected on every reconcile for good: no
// networking, nothing in the panel to say why, the reason only in a log. The
// container port is the stricter of the two, so that is the bar.
func TestPortNamesKubernetesWouldRefuseAreRejected(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	create := func(name string) int {
		t.Helper()
		return post(t, admin, srv.URL+"/api/servers", map[string]any{
			"name": "s", "template": "generic-process",
			"ports": []map[string]any{{"name": name, "port": 25565, "protocol": "TCP"}},
		}).StatusCode
	}
	for _, bad := range []string{
		"My Game Port",           // spaces and capitals
		"waaaaaaaaaaaaaytoolong", // over 15 characters
		"12345",                  // no letter
		"-lead", "trail-",        // dashes at the edges
		"under_score",
	} {
		if code := create(bad); code != http.StatusBadRequest {
			t.Errorf("port name %q accepted: %d", bad, code)
		}
	}
	for _, good := range []string{"game", "p25565", "rcon-tcp", "a", ""} {
		if code := create(good); code != http.StatusCreated {
			t.Errorf("port name %q refused: %d", good, code)
		}
	}
}

// The public address says where the operator's network sends players. An admin
// sets it, checked against the server's ports; a player with the settings
// permission cannot, or could point everyone sharing the server elsewhere; an
// empty one clears it.
func TestPublicAddressIsAdminOnlyAndChecked(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "player", "password": "playerpw12"})
	player := loginAs(t, srv.URL, "player", "playerpw12")
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{
		"name": "s", "template": "generic-process",
		"ports":  []map[string]any{{"port": 9987, "protocol": "UDP"}},
		"expose": map[string]any{"type": "NodePort"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	var created struct{ ID uint }
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	url := srv.URL + "/api/servers/" + itoa(created.ID)
	post(t, admin, url+"/access", map[string]any{"username": "player", "permissions": []string{"view", "settings"}})

	patch := func(c *http.Client, public map[string]any) int {
		t.Helper()
		resp := doPatch(t, c, url, map[string]any{"expose": map[string]any{"public": public}})
		resp.Body.Close()
		return resp.StatusCode
	}
	type exposure struct {
		Public *struct {
			Host  string
			Ports map[string]int32
		}
	}
	public := func() exposure {
		t.Helper()
		var got struct{ Expose exposure }
		getJSON(t, admin, url, &got)
		return got.Expose
	}

	good := map[string]any{"host": "ts.example.org", "ports": map[string]int{"9987/UDP": 9987}}
	if code := patch(player, good); code != http.StatusForbidden {
		t.Errorf("a player set the public address: %d", code)
	}
	for name, bad := range map[string]map[string]any{
		"a URL":                    {"host": "https://ts.example.org"},
		"a port it does not have":  {"ports": map[string]int{"25565/TCP": 25565}},
		"a port out of range":      {"ports": map[string]int{"9987/UDP": 70000}},
		"a host with a port in it": {"host": "ts.example.org:9987"},
	} {
		if code := patch(admin, bad); code != http.StatusBadRequest {
			t.Errorf("%s was accepted: %d", name, code)
		}
	}
	if code := patch(admin, good); code != http.StatusOK {
		t.Fatalf("admin patch = %d", code)
	}
	if got := public(); got.Public == nil || got.Public.Host != "ts.example.org" || got.Public.Ports["9987/UDP"] != 9987 {
		t.Errorf("public address = %+v", got.Public)
	}
	if code := patch(player, map[string]any{}); code != http.StatusForbidden {
		t.Errorf("a player cleared the public address: %d", code)
	}
	if code := patch(admin, map[string]any{}); code != http.StatusOK {
		t.Fatalf("admin clear = %d", code)
	}
	if got := public(); got.Public != nil {
		t.Errorf("an empty public address left %+v", got.Public)
	}
}

// A load balancer is an address from the operator's pool or a line on their
// cloud bill: a server's owner, or a subuser with its settings, cannot take one,
// but can keep one an admin gave and give it up.
func TestLoadBalancerIsAdminOnly(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "player", "password": "playerpw12"})
	player := loginAs(t, srv.URL, "player", "playerpw12")
	body := func(typ string) map[string]any {
		return map[string]any{
			"name": "s", "template": "generic-process", "memory": "512Mi",
			"ports":  []map[string]any{{"port": 9987, "protocol": "UDP"}},
			"expose": map[string]any{"type": typ},
		}
	}
	if r := post(t, player, srv.URL+"/api/servers", body("LoadBalancer")); r.StatusCode != http.StatusForbidden {
		t.Errorf("a player created a server behind a load balancer: %d", r.StatusCode)
	}
	r := post(t, player, srv.URL+"/api/servers", body("NodePort"))
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("a player could not create a NodePort server: %d", r.StatusCode)
	}
	var created struct{ ID uint }
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	url := srv.URL + "/api/servers/" + itoa(created.ID)

	patch := func(c *http.Client, expose map[string]any) int {
		t.Helper()
		resp := doPatch(t, c, url, map[string]any{"expose": expose})
		resp.Body.Close()
		return resp.StatusCode
	}
	exposed := func() string {
		t.Helper()
		var got struct{ Expose struct{ Type string } }
		getJSON(t, admin, url, &got)
		return got.Expose.Type
	}
	if code := patch(player, map[string]any{"type": "LoadBalancer"}); code != http.StatusForbidden {
		t.Errorf("the owner switched to a load balancer: %d", code)
	}
	if got := exposed(); got != "NodePort" {
		t.Errorf("type after a refused switch = %q", got)
	}
	if code := patch(admin, map[string]any{"type": "LoadBalancer"}); code != http.StatusOK {
		t.Fatalf("admin switch = %d", code)
	}
	if code := patch(player, map[string]any{"type": "LoadBalancer", "preserveClientIP": false}); code != http.StatusOK {
		t.Errorf("the owner could not edit a load balancer an admin gave: %d", code)
	}
	if code := patch(player, map[string]any{"type": "NodePort"}); code != http.StatusOK {
		t.Errorf("the owner could not give up the load balancer: %d", code)
	}
	if got := exposed(); got != "NodePort" {
		t.Errorf("type = %q, want NodePort", got)
	}
}
