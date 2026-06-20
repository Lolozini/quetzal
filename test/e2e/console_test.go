//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

// TestE2EConsole drives the full HTTP API against a real cluster and verifies
// the live console: stdin sent over the WebSocket reaches the container (attach)
// and its echoed output comes back through the log stream.
func TestE2EConsole(t *testing.T) {
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

	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "console.db"), Silent: true})
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

	// First-run setup creates the admin and a session cookie.
	mustStatus(t, doPost(t, hc, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}), http.StatusCreated)

	// Create + start a server from the echoing generic template.
	var created struct {
		ID        uint
		Namespace string
	}
	resp := doPost(t, hc, ts.URL+"/api/servers", map[string]any{"name": "console e2e", "template": "generic-process", "start": true})
	mustStatus(t, resp, http.StatusCreated)
	json.NewDecoder(resp.Body).Decode(&created)
	t.Cleanup(func() {
		srv, err := st.GetServer(created.ID)
		if err == nil {
			_ = rec.DeleteServer(context.Background(), srv)
		}
	})

	reconcileUntilRunning(context.Background(), t, rec, st, created.ID)

	// Dial the console WebSocket with the session cookie.
	wsURL := strings.Replace(ts.URL, "http", "ws", 1) + "/api/servers/" + itoa(created.ID) + "/console"
	hdr := http.Header{"Cookie": {cookieHeader(jar, ts.URL)}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	// Repeatedly send a command; assert it is echoed back via the log stream.
	const marker = "PINGTEST"
	stopSender := make(chan struct{})
	defer close(stopSender)
	go func() {
		msg, _ := json.Marshal(map[string]string{"type": "stdin", "data": marker})
		for {
			select {
			case <-stopSender:
				return
			default:
				_ = conn.WriteMessage(websocket.TextMessage, msg)
				time.Sleep(2 * time.Second)
			}
		}
	}()

	deadline := time.Now().Add(45 * time.Second)
	conn.SetReadDeadline(deadline)
	for time.Now().Before(deadline) {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("ws read (never saw echo): %v", err)
		}
		var m struct{ Type, Data string }
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.Type == "stdout" && strings.Contains(m.Data, "console> "+marker) {
			return // success: stdin -> container -> logs -> websocket
		}
	}
	t.Fatalf("did not observe echoed command %q within deadline", marker)
}

func itoa(u uint) string {
	b, _ := json.Marshal(u)
	return string(b)
}

func doPost(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	r, err := c.Post(url, "application/json", &buf)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return r
}

func mustStatus(t *testing.T, r *http.Response, want int) {
	t.Helper()
	if r.StatusCode != want {
		t.Fatalf("status = %d, want %d", r.StatusCode, want)
	}
}

func cookieHeader(jar http.CookieJar, rawURL string) string {
	u, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	var parts []string
	for _, c := range jar.Cookies(u.URL) {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}
