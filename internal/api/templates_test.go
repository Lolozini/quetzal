package api_test

import (
	"net/http"
	"strings"
	"testing"
)

const testEgg = `{
  "name": "Test Egg",
  "author": "a@b.c",
  "description": "an imported egg",
  "docker_images": {"Java 17": "itzg/minecraft-server"},
  "startup": "java -jar server.jar",
  "config": {"startup": "{\"done\": \"Done\"}", "stop": "stop"},
  "scripts": {"installation": {"script": "echo install", "container": "alpine:3", "entrypoint": "sh"}},
  "variables": [
    {"name": "Version", "env_variable": "VERSION", "default_value": "latest", "user_editable": true, "user_viewable": true, "rules": "required|string"}
  ]
}`

func setupAdmin(t *testing.T, ts string, c *http.Client) {
	t.Helper()
	if r := post(t, c, ts+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("setup = %d", r.StatusCode)
	}
}

func TestEggImportExportDelete(t *testing.T) {
	ts, c, _ := newTestServerStore(t)
	setupAdmin(t, ts.URL, c)

	// Import the egg (raw JSON body).
	r, err := c.Post(ts.URL+"/api/templates/import", "application/json", strings.NewReader(testEgg))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("import = %d", r.StatusCode)
	}
	r.Body.Close()

	// It appears in the catalog with its variable.
	var got struct {
		Slug      string
		Name      string
		Variables []struct{ EnvVariable string }
	}
	getJSON(t, c, ts.URL+"/api/templates/test-egg", &got)
	if got.Slug != "test-egg" || got.Name != "Test Egg" {
		t.Fatalf("imported template = %+v", got)
	}
	if len(got.Variables) != 1 || got.Variables[0].EnvVariable != "VERSION" {
		t.Errorf("variables = %+v", got.Variables)
	}

	// Export round-trips to valid JSON we can PUT back.
	exp, _ := c.Get(ts.URL + "/api/templates/test-egg/export")
	if exp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d", exp.StatusCode)
	}
	exp.Body.Close()

	// Delete (no servers use it yet).
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/templates/test-egg", nil)
	d, _ := c.Do(delReq)
	if d.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", d.StatusCode)
	}
	d.Body.Close()
	// Gone now.
	if r2, _ := c.Get(ts.URL + "/api/templates/test-egg"); r2.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", r2.StatusCode)
	}
}

func TestReinstall(t *testing.T) {
	ts, c, st := newTestServerStore(t)
	setupAdmin(t, ts.URL, c)

	// A server from an egg WITH an install script can be reinstalled.
	r, _ := c.Post(ts.URL+"/api/templates/import", "application/json", strings.NewReader(testEgg))
	r.Body.Close()
	cr := post(t, c, ts.URL+"/api/servers", map[string]any{"name": "egg server", "template": "test-egg"})
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", cr.StatusCode)
	}
	var servers []struct {
		ID                uint
		InstallGeneration int
	}
	getJSON(t, c, ts.URL+"/api/servers", &servers)
	if len(servers) != 1 || servers[0].InstallGeneration != 1 {
		t.Fatalf("server install generation = %+v, want 1", servers)
	}
	id := servers[0].ID

	if rr := post(t, c, ts.URL+"/api/servers/"+itoa(id)+"/reinstall", map[string]any{"wipeData": true}); rr.StatusCode != http.StatusOK {
		t.Fatalf("reinstall = %d", rr.StatusCode)
	}
	srv, _ := st.GetServer(id)
	if srv.InstallGeneration != 2 {
		t.Errorf("generation after reinstall = %d, want 2", srv.InstallGeneration)
	}
	if !srv.InstallWipe {
		t.Error("wipe flag should be set")
	}

	// A server whose template has no install script can't be reinstalled.
	cr2 := post(t, c, ts.URL+"/api/servers", map[string]any{"name": "plain", "template": "generic-process"})
	if cr2.StatusCode != http.StatusCreated {
		t.Fatalf("create plain = %d", cr2.StatusCode)
	}
	getJSON(t, c, ts.URL+"/api/servers", &servers)
	var plainID uint
	for _, s := range servers {
		if s.ID != id {
			plainID = s.ID
		}
	}
	if rr := post(t, c, ts.URL+"/api/servers/"+itoa(plainID)+"/reinstall", map[string]any{}); rr.StatusCode != http.StatusBadRequest {
		t.Errorf("reinstall without install script = %d, want 400", rr.StatusCode)
	}
}

func TestEggDeleteBlockedWhileInUse(t *testing.T) {
	ts, c, _ := newTestServerStore(t)
	setupAdmin(t, ts.URL, c)

	r, _ := c.Post(ts.URL+"/api/templates/import", "application/json", strings.NewReader(testEgg))
	r.Body.Close()

	// Create a server from it.
	if cr := post(t, c, ts.URL+"/api/servers", map[string]any{"name": "uses egg", "template": "test-egg"}); cr.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d", cr.StatusCode)
	}
	// Deleting the template is now refused.
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/templates/test-egg", nil)
	d, _ := c.Do(delReq)
	if d.StatusCode != http.StatusConflict {
		t.Fatalf("delete in-use = %d, want 409", d.StatusCode)
	}
	d.Body.Close()
}
