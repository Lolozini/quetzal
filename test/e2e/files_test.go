//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

// TestE2EFiles drives the file-manager HTTP API against a real running pod:
// write -> list -> read -> mkdir -> rename -> read, plus a path-traversal probe.
func TestE2EFiles(t *testing.T) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	crClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("ctrl client: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "files.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := reconciler.New(crClient, st)

	ts := httptest.NewServer(api.New(st, cs, cfg).Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	mustStatus(t, doPost(t, hc, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}), http.StatusCreated)

	var created struct{ ID uint }
	resp := doPost(t, hc, ts.URL+"/api/servers", map[string]any{"name": "files e2e", "template": "generic-process", "start": true})
	mustStatus(t, resp, http.StatusCreated)
	json.NewDecoder(resp.Body).Decode(&created)
	t.Cleanup(func() {
		if srv, err := st.GetServer(created.ID); err == nil {
			_ = rec.DeleteServer(context.Background(), srv)
		}
	})
	reconcileUntilRunning(context.Background(), t, rec, st, created.ID)

	base := ts.URL + "/api/servers/" + itoa(created.ID) + "/files"
	const content = "hello quetzal"

	// Write a file.
	mustStatus(t, doFile(t, hc, http.MethodPut, base+"/content?path=hello.txt", content), http.StatusNoContent)

	// List the data root: hello.txt with the right size.
	var entries []struct {
		Name string
		Size int64
		Dir  bool
	}
	r := doFile(t, hc, http.MethodGet, base+"?path=", "")
	mustStatus(t, r, http.StatusOK)
	json.NewDecoder(r.Body).Decode(&entries)
	found := false
	for _, e := range entries {
		if e.Name == "hello.txt" {
			found = true
			if e.Size != int64(len(content)) {
				t.Errorf("hello.txt size = %d, want %d", e.Size, len(content))
			}
		}
	}
	if !found {
		t.Fatalf("hello.txt not in listing: %+v", entries)
	}

	// Read it back.
	if got := readBody(t, doFile(t, hc, http.MethodGet, base+"/content?path=hello.txt", "")); got != content {
		t.Errorf("read = %q, want %q", got, content)
	}

	// mkdir + rename into it + read.
	mustStatus(t, doFile(t, hc, http.MethodPost, base+"/mkdir?path=sub", ""), http.StatusNoContent)
	mustStatus(t, doFile(t, hc, http.MethodPost, base+"/rename?path=hello.txt&to=sub/moved.txt", ""), http.StatusNoContent)
	if got := readBody(t, doFile(t, hc, http.MethodGet, base+"/content?path=sub/moved.txt", "")); got != content {
		t.Errorf("after rename read = %q, want %q", got, content)
	}

	// Download the directory as a gzip tarball (folder download).
	arc := doFile(t, hc, http.MethodGet, base+"/archive?path=sub", "")
	mustStatus(t, arc, http.StatusOK)
	ab := readBody(t, arc)
	if len(ab) < 2 || ab[0] != 0x1f || ab[1] != 0x8b {
		t.Errorf("archive is not a gzip stream (len=%d)", len(ab))
	}

	// Path traversal must be confined to the data root: reading ../../etc/passwd
	// resolves under /data (nonexistent) and must NOT return the real file.
	tr := doFile(t, hc, http.MethodGet, base+"/content?path=../../../../etc/passwd", "")
	body := readBody(t, tr)
	if strings.Contains(body, "root:") {
		t.Fatalf("path traversal escaped the data root: %q", body)
	}

	// Delete the directory.
	mustStatus(t, doFile(t, hc, http.MethodDelete, base+"?path=sub", ""), http.StatusNoContent)
}

func doFile(t *testing.T, c *http.Client, method, url, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rdr)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func readBody(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}
