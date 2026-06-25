package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/notify"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

func TestNotificationChannelCRUDAndMasking(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// Create a Discord channel: its url is a secret and must never be echoed.
	r := post(t, admin, srv.URL+"/api/notifications/channels", map[string]any{
		"name": "alerts", "type": "discord", "enabled": true, "serverId": 0,
		"config": map[string]string{"url": "https://discord.test/webhook/abc"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create discord channel = %d", r.StatusCode)
	}
	var created struct {
		ID      uint
		Config  map[string]string
		Secrets map[string]bool
	}
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	if _, leaked := created.Config["url"]; leaked {
		t.Error("discord url leaked in config (must be masked)")
	}
	if !created.Secrets["url"] {
		t.Error("expected secrets.url=true")
	}

	// Email channel: public fields are returned, password is masked.
	r = post(t, admin, srv.URL+"/api/notifications/channels", map[string]any{
		"name": "mail", "type": "email", "enabled": true, "serverId": 0,
		"config": map[string]string{
			"host": "smtp.test", "port": "587", "from": "q@test", "to": "ops@test",
			"username": "q", "password": "hunter2", "tls": "starttls",
		},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create email channel = %d", r.StatusCode)
	}
	var mail struct {
		Config  map[string]string
		Secrets map[string]bool
	}
	json.NewDecoder(r.Body).Decode(&mail)
	r.Body.Close()
	if mail.Config["host"] != "smtp.test" || mail.Config["from"] != "q@test" {
		t.Errorf("email public config not returned: %+v", mail.Config)
	}
	if _, leaked := mail.Config["password"]; leaked {
		t.Error("smtp password leaked")
	}
	if !mail.Secrets["password"] {
		t.Error("expected secrets.password=true")
	}

	// Missing required config is rejected.
	if r := post(t, admin, srv.URL+"/api/notifications/channels", map[string]any{
		"name": "bad", "type": "webhook", "enabled": true, "config": map[string]string{},
	}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("webhook without url = %d, want 400", r.StatusCode)
	}

	// A non-admin cannot manage global channels.
	createUser(t, admin, srv.URL, map[string]any{"username": "bob", "password": "bobpw1234"})
	bob := loginAs(t, srv.URL, "bob", "bobpw1234")
	if r := post(t, bob, srv.URL+"/api/notifications/channels", map[string]any{
		"name": "x", "type": "discord", "config": map[string]string{"url": "https://x.test"},
	}); r.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin global channel create = %d, want 403", r.StatusCode)
	}
	if r, _ := bob.Get(srv.URL + "/api/notifications/channels"); r.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin list channels = %d, want 403", r.StatusCode)
	}
}

// TestNotificationDeliveryEndToEnd verifies an API action emits an event that
// the dispatcher delivers to a configured webhook channel.
func TestNotificationDeliveryEndToEnd(t *testing.T) {
	// Webhook receiver capturing delivered event types.
	var mu sync.Mutex
	got := map[string]bool{}
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Type string `json:"type"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		got[p.Type] = true
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer recv.Close()

	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "n.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	apiSrv := api.New(st, fake.NewSimpleClientset(), &rest.Config{})
	d := notify.New(st)
	d.Interval = 20 * time.Millisecond
	d.Client = recv.Client() // permissive: this test targets a loopback receiver
	apiSrv.Dispatch = d
	ts := httptest.NewServer(apiSrv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post(t, c, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// A global, catch-all webhook channel.
	if r := post(t, c, ts.URL+"/api/notifications/channels", map[string]any{
		"name": "hook", "type": "webhook", "enabled": true, "serverId": 0,
		"config": map[string]string{"url": recv.URL},
	}); r.StatusCode != http.StatusCreated {
		t.Fatalf("create channel = %d", r.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// An audited action -> event -> delivery.
	if r := post(t, c, ts.URL+"/api/apikeys", map[string]string{"name": "k"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("create apikey = %d", r.StatusCode)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := got["apikey.create"]
		mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("webhook never received apikey.create event")
}
