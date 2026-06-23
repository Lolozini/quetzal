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
