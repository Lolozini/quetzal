package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(Config{
		Driver:    DriverSQLite,
		DSN:       dsn,
		Silent:    true,
		SecretKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestSetDesiredStateKeepsStatus(t *testing.T) {
	s := newTestStore(t)
	srv := &models.Server{Slug: "p", Namespace: "ns", DesiredState: models.StateRunning}
	if err := s.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Controller writes status...
	if err := s.UpdateServerStatus(srv.ID, models.Status{Phase: models.PhaseRunning}); err != nil {
		t.Fatalf("status: %v", err)
	}
	// ...then a power action changes only desiredState.
	if err := s.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatalf("set state: %v", err)
	}
	got, _ := s.GetServer(srv.ID)
	if got.DesiredState != models.StateStopped {
		t.Errorf("desiredState = %q, want Stopped", got.DesiredState)
	}
	if got.Status.Phase != models.PhaseRunning {
		t.Errorf("status was clobbered: phase = %q, want Running", got.Status.Phase)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	s := newTestStore(t)
	_ = s.CreateSession(&models.Session{Token: "old", UserID: 1, ExpiresAt: time.Now().Add(-time.Hour)})
	_ = s.CreateSession(&models.Session{Token: "new", UserID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	n, err := s.DeleteExpiredSessions()
	if err != nil || n != 1 {
		t.Fatalf("deleted = %d, err = %v, want 1", n, err)
	}
	if _, err := s.GetSession("old"); err != ErrNotFound {
		t.Error("expired session should be gone")
	}
	if _, err := s.GetSession("new"); err != nil {
		t.Error("valid session should remain")
	}
}

func TestAllocateNodePortStableAndUnique(t *testing.T) {
	s := newTestStore(t)

	// Two ports on the same server get distinct, in-range allocations.
	a, err := s.AllocateNodePort(1, "game", 30000, 30002)
	if err != nil {
		t.Fatalf("alloc game: %v", err)
	}
	b, err := s.AllocateNodePort(1, "query", 30000, 30002)
	if err != nil {
		t.Fatalf("alloc query: %v", err)
	}
	if a == b {
		t.Fatalf("ports collided: %d == %d", a, b)
	}
	if a < 30000 || a > 30002 || b < 30000 || b > 30002 {
		t.Fatalf("ports out of range: %d, %d", a, b)
	}

	// Re-allocating the same (server, name) is stable.
	again, err := s.AllocateNodePort(1, "game", 30000, 30002)
	if err != nil || again != a {
		t.Fatalf("alloc not stable: %d != %d (err %v)", again, a, err)
	}

	// A second server takes the last free port; the range is then exhausted.
	c, err := s.AllocateNodePort(2, "game", 30000, 30002)
	if err != nil {
		t.Fatalf("alloc server2: %v", err)
	}
	if c == a || c == b {
		t.Fatalf("server2 port collided: %d", c)
	}
	if _, err := s.AllocateNodePort(3, "game", 30000, 30002); err == nil {
		t.Fatal("expected exhaustion error when pool is full")
	}

	// Releasing server 1 frees its two ports for reuse.
	if err := s.ReleaseServerPorts(1); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.AllocateNodePort(3, "game", 30000, 30002); err != nil {
		t.Fatalf("alloc after release: %v", err)
	}
}

func TestDeleteServerReleasesPorts(t *testing.T) {
	s := newTestStore(t)
	srv := &models.Server{Slug: "p", Namespace: "ns"}
	if err := s.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AllocateNodePort(srv.ID, "game", 30000, 30000); err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if err := s.DeleteServer(srv.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The single port in the range must be free again.
	if _, err := s.AllocateNodePort(99, "game", 30000, 30000); err != nil {
		t.Fatalf("port not released on delete: %v", err)
	}
}

func TestSealOpenSecrets(t *testing.T) {
	s := newTestStore(t)
	m := map[string]string{"RCON_PASSWORD": "hunter2"}
	blob, err := s.SealSecrets(m)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if blob == "" || strings.Contains(blob, "hunter2") {
		t.Fatalf("sealed blob is empty or leaks plaintext: %q", blob)
	}
	got, err := s.OpenSecrets(blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got["RCON_PASSWORD"] != "hunter2" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// Empty map -> empty blob.
	if b, _ := s.SealSecrets(nil); b != "" {
		t.Errorf("empty map should seal to empty string, got %q", b)
	}
}

func TestServerCRUDAndStatusSerializer(t *testing.T) {
	s := newTestStore(t)

	srv := &models.Server{
		Slug:         "demo",
		DisplayName:  "Demo",
		TemplateID:   1,
		Namespace:    "quetzal-srv-demo",
		DesiredState: models.StateRunning,
		Env:          map[string]string{"FOO": "bar"},
		Storage:      models.Storage{Type: models.StoragePVC, Size: "5Gi"},
	}
	if err := s.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}

	// This is the path that previously failed: a serialized struct column.
	st := models.Status{Phase: models.PhaseRunning, Endpoints: []string{"server.ns.svc:25565"}}
	if err := s.UpdateServerStatus(srv.ID, st); err != nil {
		t.Fatalf("update status: %v", err)
	}

	got, err := s.GetServerBySlug("demo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != models.PhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if len(got.Status.Endpoints) != 1 || got.Status.Endpoints[0] != "server.ns.svc:25565" {
		t.Errorf("endpoints = %+v", got.Status.Endpoints)
	}
	if got.Env["FOO"] != "bar" {
		t.Errorf("env not round-tripped: %+v", got.Env)
	}
}

func TestUpsertTemplateBumpsVersion(t *testing.T) {
	s := newTestStore(t)
	tmpl := &models.Template{Slug: "x", Name: "X"}
	saved, err := s.UpsertTemplate(tmpl)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if saved.Version != 1 {
		t.Errorf("version = %d, want 1", saved.Version)
	}
	again, err := s.UpsertTemplate(&models.Template{Slug: "x", Name: "X v2"})
	if err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	if again.Version != 2 {
		t.Errorf("version = %d, want 2", again.Version)
	}
}
