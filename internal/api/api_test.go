package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "api.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := api.New(st, fake.NewSimpleClientset(), &rest.Config{}).Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar}
}

func post(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	resp, err := c.Post(url, "application/json", &buf)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func TestAPIFlow(t *testing.T) {
	ts, c := newTestServer(t)

	// setup needed initially
	var status struct{ Needed bool }
	getJSON(t, c, ts.URL+"/api/setup/status", &status)
	if !status.Needed {
		t.Fatal("setup should be needed initially")
	}

	// create admin
	if r := post(t, c, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("setup status = %d", r.StatusCode)
	}

	// setup no longer needed; second setup forbidden
	getJSON(t, c, ts.URL+"/api/setup/status", &status)
	if status.Needed {
		t.Error("setup should no longer be needed")
	}
	if r := post(t, c, ts.URL+"/api/setup", map[string]string{"username": "x", "password": "supersecret"}); r.StatusCode != http.StatusConflict {
		t.Errorf("second setup = %d, want 409", r.StatusCode)
	}

	// authenticated /me works (cookie from setup)
	if r, _ := c.Get(ts.URL + "/api/me"); r.StatusCode != http.StatusOK {
		t.Errorf("me = %d, want 200", r.StatusCode)
	}

	// create a server from a built-in template
	var created struct{ ID uint }
	r := post(t, c, ts.URL+"/api/servers", map[string]any{"name": "My Test", "template": "generic-process", "start": true})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	if created.ID == 0 {
		t.Fatal("created server has no id")
	}

	// list
	var servers []map[string]any
	getJSON(t, c, ts.URL+"/api/servers", &servers)
	if len(servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(servers))
	}

	// power stop
	url := ts.URL + "/api/servers/" + itoa(created.ID)
	if r := post(t, c, url+"/power", map[string]string{"action": "stop"}); r.StatusCode != http.StatusOK {
		t.Errorf("stop = %d", r.StatusCode)
	}
	var srv map[string]any
	getJSON(t, c, url, &srv)
	if srv["desiredState"] != "Stopped" {
		t.Errorf("desiredState = %v, want Stopped", srv["desiredState"])
	}

	// delete
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	dr, err := c.Do(req)
	if err != nil || dr.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %v / %d", err, dr.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(t)
	noauth := &http.Client{} // no cookie jar
	if r, _ := noauth.Get(ts.URL + "/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("me without auth = %d, want 401", r.StatusCode)
	}
	if r, _ := noauth.Get(ts.URL + "/api/servers"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("servers without auth = %d, want 401", r.StatusCode)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	ts, c := newTestServer(t)
	post(t, c, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	fresh := &http.Client{}
	if r := post(t, fresh, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "wrong"}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong login = %d, want 401", r.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	fresh.Jar = jar
	if r := post(t, fresh, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret"}); r.StatusCode != http.StatusOK {
		t.Errorf("correct login = %d, want 200", r.StatusCode)
	}
}

func getJSON(t *testing.T, c *http.Client, url string, v any) {
	t.Helper()
	r, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func itoa(u uint) string {
	return strconv.FormatUint(uint64(u), 10)
}
