package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// restic ends a missing-repository error by pointing at the repository, and
// the last line, redacted, was all a failed restore said: "the backup
// repository". The line that says what went wrong is the Fatal one.
func TestFailureMessageSaysWhatWentWrong(t *testing.T) {
	repo := "s3:https://s3.example.cloud/ops-backups/prod/mc-a1b2"
	logs := "Fatal: repository does not exist: unable to open config file: Stat: The specified key does not exist.\n" +
		"Is there a repository at the following location?\n" + repo + "\n"
	got := failureMessage(logs, repo)
	if !strings.Contains(got, "repository does not exist") || strings.Contains(got, "ops-backups") {
		t.Errorf("message = %q, want the Fatal line with the location redacted", got)
	}
	// Without a Fatal line, the last line that says something.
	if got := failureMessage("restoring\nno space left on device\n", repo); got != "no space left on device" {
		t.Errorf("message = %q", got)
	}
	if got := failureMessage("Is there a repository at the following location?\n"+strings.TrimPrefix(repo, "s3:")+"\n", repo); got != "" {
		t.Errorf("a pointer alone became the message: %q", got)
	}
}

// A target is where its snapshots are: the endpoint, the bucket and the
// prefix. How it is reached (TLS, region) does not move them.
func TestTargetIDIsTheLocation(t *testing.T) {
	base := models.BackupConfig{Endpoint: "s3.example", Bucket: "b", Prefix: "games"}
	same := []models.BackupConfig{
		{Endpoint: "S3.Example ", Bucket: "b", Prefix: "/games/"},
		{Endpoint: "s3.example", Bucket: "b", Prefix: "games", UseSSL: true, Region: "fr-par"},
	}
	for _, c := range same {
		if TargetID(&c) != TargetID(&base) {
			t.Errorf("%+v is the same place as %+v", c, base)
		}
	}
	other := []models.BackupConfig{
		{Endpoint: "s3.example", Bucket: "c", Prefix: "games"},
		{Endpoint: "s3.example", Bucket: "b", Prefix: "other"},
		{Endpoint: "s3.other", Bucket: "b", Prefix: "games"},
	}
	for _, c := range other {
		if TargetID(&c) == TargetID(&base) {
			t.Errorf("%+v is not the same place as %+v", c, base)
		}
	}
}

// The manager records the target a backup goes to, refuses to restore one made
// to another, and drops a backup of another target without a snapshot deletion
// that could only fail.
func TestManagerKnowsWhereABackupWent(t *testing.T) {
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "m.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := &models.BackupConfig{Endpoint: "s3.example", Bucket: "now", KeepLast: 3}
	if err := st.SaveBackupConfig(cfg, "ak", "sk", "rp"); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv := &models.Server{Slug: "mc", Namespace: "quetzal-srv-mc"}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("server: %v", err)
	}
	m := NewManager(st, cluster.New(st, cluster.Clients{Clientset: fake.NewSimpleClientset()}))
	ctx := context.Background()
	elsewhere := TargetID(&models.BackupConfig{Endpoint: "s3.example", Bucket: "before"})

	fresh := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	old := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded, Target: elsewhere}
	for _, b := range []*models.Backup{fresh, old} {
		if err := st.CreateBackup(b); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	m.processPending(ctx)
	if got, _ := st.GetBackup(fresh.ID); got.Phase != models.BackupRunning || got.Target != TargetID(cfg) {
		t.Errorf("a started backup = %s, target %q; want Running on %q", got.Phase, got.Target, TargetID(cfg))
	}

	// Its Job succeeds, clearing the way for a restore of the old backup.
	done, _ := st.GetBackup(fresh.ID)
	done.Phase = models.BackupSucceeded
	if err := st.UpdateBackup(done); err != nil {
		t.Fatalf("finish: %v", err)
	}
	restore := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending, SourceID: old.ID}
	if err := st.CreateBackup(restore); err != nil {
		t.Fatalf("seed restore: %v", err)
	}
	m.processPending(ctx)
	if got, _ := st.GetBackup(restore.ID); got.Phase != models.BackupFailed || got.Message != OtherTargetMessage {
		t.Errorf("restoring a backup of another target = %s %q, want Failed saying why", got.Phase, got.Message)
	}

	if err := st.MarkBackupDeleting(old.ID); err != nil {
		t.Fatalf("mark: %v", err)
	}
	m.processDeleting(ctx)
	if _, err := st.GetBackup(old.ID); err == nil {
		t.Error("a backup of another target waits on a snapshot deletion that cannot run")
	}
}
