package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/reconciler"
)

// A path that climbs out of the data directory used to be brought back into it
// without a word: "../escape.txt" was written as "escape.txt" at the top of the
// server's files, a name nobody had asked for. Confined, so harmless, but the
// caller believed the file was somewhere else. Such a path is now refused, on
// every route that takes one, before anything runs in the container.
func TestAPathThatLeavesTheDataDirectoryIsRefused(t *testing.T) {
	ts, c, st, apiSrv, cs := newTestServerFull(t)
	apiSrv.DataReadyTimeout = 50 * time.Millisecond
	setupAdmin(t, ts.URL, c)
	var created struct{ ID uint }
	r := post(t, c, ts.URL+"/api/servers", map[string]any{"name": "files", "template": "generic-process", "memory": "512Mi"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	_ = json.NewDecoder(r.Body).Decode(&created)
	srv, err := st.GetServer(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A running data manager, so that the routes reading a JSON body get as far
	// as reading it.
	_, err = cs.CoreV1().Pods(srv.Namespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "data-1", Namespace: srv.Namespace, Labels: map[string]string{reconciler.DataLabel: srv.Slug}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: reconciler.WorkloadName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/servers/" + itoa(created.ID) + "/files"
	send := func(method, u string, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, u, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	q := url.QueryEscape
	for _, tc := range []struct{ name, method, url, body string }{
		{"write", "PUT", base + "/content?path=" + q("../escape.txt"), "hello"},
		{"write, deeper", "PUT", base + "/content?path=" + q("config/../../escape.txt"), "hello"},
		{"list", "GET", base + "?path=" + q("/.."), ""},
		{"read", "GET", base + "/content?path=" + q("../etc/passwd"), ""},
		{"rename target", "POST", base + "/rename?path=a.txt&to=" + q("../a.txt"), ""},
		{"move destination", "POST", base + "/move", `{"root": "", "files": ["a.txt"], "destination": "../elsewhere"}`},
		{"bulk delete root", "POST", base + "/delete", `{"root": "..", "files": ["a.txt"]}`},
		{"copy", "POST", base + "/copy", `{"path": "../a.txt"}`},
		{"compress root", "POST", base + "/compress", `{"root": "../x", "files": ["a.txt"]}`},
		{"decompress", "POST", base + "/decompress", `{"path": "../a.zip"}`},
	} {
		code, body := send(tc.method, tc.url, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, "escapes the data directory") {
			t.Errorf("%s: %d %s, want 400 saying the path escapes", tc.name, code, strings.TrimSpace(body))
		}
	}
}
