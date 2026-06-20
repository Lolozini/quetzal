package store

import (
	"path/filepath"
	"strings"
	"testing"

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
