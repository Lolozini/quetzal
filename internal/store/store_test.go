package store

import (
	"errors"
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

func TestIsConcurrentMigrationError(t *testing.T) {
	for _, s := range []string{
		"SQL logic error: duplicate column name: install_generation (1)",
		"ERROR: column \"x\" of relation \"servers\" already exists (SQLSTATE 42701)",
	} {
		if !isConcurrentMigrationError(errors.New(s)) {
			t.Errorf("expected concurrent-migration match for %q", s)
		}
	}
	for _, s := range []string{"connection refused", "no such table: servers", ""} {
		if isConcurrentMigrationError(errors.New(s)) {
			t.Errorf("did not expect match for %q", s)
		}
	}
	if isConcurrentMigrationError(nil) {
		t.Error("nil should not match")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newTestStore(t) // already migrates once
	if err := s.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestGetUserByEmailCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateUser(&models.User{Username: "ann", PasswordHash: "x", Email: "Ann@Example.com"}); err != nil {
		t.Fatal(err)
	}
	if u, err := s.GetUserByEmail("ann@example.com"); err != nil || u.Username != "ann" {
		t.Fatalf("lookup = %v, %v", u, err)
	}
	if _, err := s.GetUserByEmail(""); err != ErrNotFound {
		t.Error("empty email should not match")
	}
	if err := s.UpdateUserEmail(1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUserByEmail("ann@example.com"); err != ErrNotFound {
		t.Error("email should be cleared")
	}
}

func TestPasswordResetTokensAndSessions(t *testing.T) {
	s := newTestStore(t)
	_ = s.CreateSession(&models.Session{Token: "a", UserID: 7, ExpiresAt: time.Now().Add(time.Hour)})
	_ = s.CreateSession(&models.Session{Token: "b", UserID: 7, ExpiresAt: time.Now().Add(time.Hour)})
	_ = s.CreatePasswordReset(&models.PasswordReset{UserID: 7, TokenHash: "live", ExpiresAt: time.Now().Add(time.Hour)})
	_ = s.CreatePasswordReset(&models.PasswordReset{UserID: 7, TokenHash: "stale", ExpiresAt: time.Now().Add(-time.Hour)})

	if pr, err := s.GetPasswordResetByHash("live"); err != nil || pr.UserID != 7 {
		t.Fatalf("get live = %v, %v", pr, err)
	}
	if n, err := s.DeleteExpiredPasswordResets(); err != nil || n != 1 {
		t.Fatalf("expired purge = %d, %v, want 1", n, err)
	}
	if err := s.DeleteSessionsForUser(7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession("a"); err != ErrNotFound {
		t.Error("sessions should be cleared for the user")
	}
	if err := s.DeletePasswordResetsForUser(7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPasswordResetByHash("live"); err != ErrNotFound {
		t.Error("reset tokens should be cleared for the user")
	}
}

func TestSMTPConfigSeal(t *testing.T) {
	s := newTestStore(t)
	if cfg, err := s.GetSMTPConfig(); err != nil || len(cfg) != 0 {
		t.Fatalf("empty = %v, %v", cfg, err)
	}
	in := map[string]string{"host": "smtp.example", "from": "a@b", "password": "s3cret"}
	if err := s.SetSMTPConfig(in); err != nil {
		t.Fatal(err)
	}
	// Stored value must not be clear text (a SecretKey is configured).
	blob, _ := s.GetSetting(SettingSMTP)
	if strings.Contains(blob, "s3cret") {
		t.Error("SMTP password stored in clear text")
	}
	got, err := s.GetSMTPConfig()
	if err != nil || got["password"] != "s3cret" || got["host"] != "smtp.example" {
		t.Fatalf("round-trip = %v, %v", got, err)
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

	// ReleaseNodePort frees just one named allocation (e.g. SFTP), leaving the
	// server's other ports intact.
	if err := s.ReleaseNodePort(1, "query"); err != nil {
		t.Fatalf("release one: %v", err)
	}
	if got, err := s.AllocateNodePort(1, "game", 30000, 30002); err != nil || got != a {
		t.Fatalf("game port should be untouched after releasing query: got %d err %v", got, err)
	}
	if _, err := s.AllocateNodePort(3, "game", 30000, 30002); err != nil {
		t.Fatalf("freed query port should be reusable: %v", err)
	}

	// Releasing server 1 frees its remaining ports for reuse.
	if err := s.ReleaseServerPorts(1); err != nil {
		t.Fatalf("release: %v", err)
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

func TestBackupConfigEncryptsAndKeepsSecretsOnUpdate(t *testing.T) {
	s := newTestStore(t)
	cfg := &models.BackupConfig{Endpoint: "minio:9000", Bucket: "b", KeepLast: 5}
	if err := s.SaveBackupConfig(cfg, "AKIA", "s3kr3t", "restic-pw"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetBackupConfig()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Stored encrypted, not plaintext.
	if got.SecretKeyEnc == "" || strings.Contains(got.SecretKeyEnc, "s3kr3t") {
		t.Fatalf("secret key not encrypted: %q", got.SecretKeyEnc)
	}
	ak, sk, pw, err := s.BackupSecrets(got)
	if err != nil || ak != "AKIA" || sk != "s3kr3t" || pw != "restic-pw" {
		t.Fatalf("round-trip = %q/%q/%q err=%v", ak, sk, pw, err)
	}

	// Update non-secret fields with empty secrets -> previous secrets kept.
	if err := s.SaveBackupConfig(&models.BackupConfig{Endpoint: "minio:9000", Bucket: "b2", KeepLast: 9}, "", "", ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetBackupConfig()
	if got.Bucket != "b2" || got.KeepLast != 9 {
		t.Errorf("non-secret update not applied: %+v", got)
	}
	ak, sk, pw, _ = s.BackupSecrets(got)
	if ak != "AKIA" || sk != "s3kr3t" || pw != "restic-pw" {
		t.Errorf("secrets should be preserved on update: %q/%q/%q", ak, sk, pw)
	}
}

func TestPruneBackupsKeepsNewest(t *testing.T) {
	s := newTestStore(t)
	srv := &models.Server{Slug: "p", Namespace: "ns"}
	_ = s.CreateServer(srv)
	for i := 0; i < 5; i++ {
		_ = s.CreateBackup(&models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded})
	}
	if err := s.PruneBackups(srv.ID, 2); err != nil {
		t.Fatalf("prune: %v", err)
	}
	bs, _ := s.ListBackupsForServer(srv.ID)
	if len(bs) != 2 {
		t.Fatalf("kept %d backups, want 2", len(bs))
	}
	// Newest (highest IDs) retained.
	if bs[0].ID < bs[1].ID {
		t.Error("list should be newest-first")
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

func TestEnsureLocalClusterIdempotentAndAdopts(t *testing.T) {
	s := newTestStore(t)

	// A pre-multi-cluster server (ClusterID 0) should be adopted onto the local
	// cluster the first time EnsureLocalCluster runs.
	legacy := &models.Server{Slug: "legacy", Namespace: "ns-legacy", DesiredState: models.StateStopped}
	if err := s.CreateServer(legacy); err != nil {
		t.Fatalf("create server: %v", err)
	}

	local1, err := s.EnsureLocalCluster()
	if err != nil {
		t.Fatalf("ensure local (1): %v", err)
	}
	local2, err := s.EnsureLocalCluster()
	if err != nil {
		t.Fatalf("ensure local (2): %v", err)
	}
	if local1.ID != local2.ID {
		t.Errorf("local cluster id changed: %d != %d", local1.ID, local2.ID)
	}
	if !local1.InCluster {
		t.Errorf("local cluster should be InCluster")
	}

	clusters, err := s.ListClusters()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	inCluster := 0
	for _, c := range clusters {
		if c.InCluster {
			inCluster++
		}
	}
	if inCluster != 1 {
		t.Errorf("InCluster cluster count = %d, want 1", inCluster)
	}

	got, _ := s.GetServer(legacy.ID)
	if got.ClusterID != local1.ID {
		t.Errorf("legacy server adopted to cluster %d, want %d", got.ClusterID, local1.ID)
	}
}

func TestClusterKubeconfigEncrypted(t *testing.T) {
	s := newTestStore(t)
	const kubeconfig = "apiVersion: v1\nkind: Config\n# secret creds here\n"
	c := &models.Cluster{Slug: "edge", Name: "edge"}
	if err := s.CreateCluster(c, kubeconfig); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	stored, err := s.GetCluster(c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.Contains(stored.KubeconfigEnc, "secret creds") {
		t.Errorf("kubeconfig stored in clear text: %q", stored.KubeconfigEnc)
	}
	back, err := s.ClusterKubeconfig(stored)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if back != kubeconfig {
		t.Errorf("round-trip mismatch: %q != %q", back, kubeconfig)
	}
}
