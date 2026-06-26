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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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

// TestE2EOfflineFiles proves files can be managed while the server is STOPPED.
// The always-on data-manager pod mounts the data volume permanently (the game
// pod is co-located with it via podAffinity), so file operations work whether the
// game is running or stopped, and the data-manager persists across power cycles.
func TestE2EOfflineFiles(t *testing.T) {
	ctx := context.Background()
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
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "offline.db"), Silent: true})
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
	resp := doPost(t, hc, ts.URL+"/api/servers", map[string]any{"name": "offline e2e", "template": "generic-process", "start": true})
	mustStatus(t, resp, http.StatusCreated)
	json.NewDecoder(resp.Body).Decode(&created)
	t.Cleanup(func() {
		if srv, err := st.GetServer(created.ID); err == nil {
			_ = rec.DeleteServer(ctx, srv)
		}
	})
	reconcileUntilRunning(ctx, t, rec, st, created.ID)

	srv, _ := st.GetServer(created.ID)
	ns := srv.Namespace
	base := ts.URL + "/api/servers/" + itoa(created.ID) + "/files"

	// Stop the server, then reconcile until the GAME pod is gone. The data-manager
	// pod stays up (it holds the volume for files/SFTP).
	mustStatus(t, doPost(t, hc, ts.URL+"/api/servers/"+itoa(created.ID)+"/power", map[string]string{"action": "stop"}), http.StatusOK)
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		_ = rec.ReconcileServer(ctx, created.ID)
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: reconciler.ServerLabel + "=" + srv.Slug})
		if err != nil {
			return false, nil
		}
		return len(pods.Items) == 0, nil
	}); err != nil {
		t.Fatalf("game pod never terminated after stop: %v", err)
	}

	// The data-manager pod must exist (DataLabel, not the workload label).
	dp, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: reconciler.DataLabel + "=" + srv.Slug})
	if err != nil || len(dp.Items) != 1 {
		t.Fatalf("data-manager pod = %v (err %v), want exactly 1", len(dp.Items), err)
	}
	if _, ok := dp.Items[0].Labels[reconciler.ServerLabel]; ok {
		t.Error("data-manager pod must not carry the workload label")
	}

	// Offline write -> read-back via the data-manager pod.
	const content = "offline edit"
	mustStatus(t, doFile(t, hc, http.MethodPut, base+"/content?path=offline.txt", content), http.StatusNoContent)
	if got := readBody(t, doFile(t, hc, http.MethodGet, base+"/content?path=offline.txt", "")); got != content {
		t.Errorf("offline read = %q, want %q", got, content)
	}

	// Start the server again; the workload reaches Running and the data-manager
	// persists across the power cycle (it is not torn down).
	mustStatus(t, doPost(t, hc, ts.URL+"/api/servers/"+itoa(created.ID)+"/power", map[string]string{"action": "start"}), http.StatusOK)
	reconcileUntilRunning(ctx, t, rec, st, created.ID)
	dp2, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: reconciler.DataLabel + "=" + srv.Slug})
	if err != nil || len(dp2.Items) != 1 {
		t.Fatalf("data-manager pod after start = %v (err %v), want exactly 1 (it persists)", len(dp2.Items), err)
	}

	// The offline-written file survived the restart (same data volume).
	if got := readBody(t, doFile(t, hc, http.MethodGet, base+"/content?path=offline.txt", "")); got != content {
		t.Errorf("post-restart read = %q, want %q", got, content)
	}
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
