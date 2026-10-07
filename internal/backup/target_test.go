package backup

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/testdb"
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
	// restic 0.18 and later say it in JSON when the command runs with --json,
	// as the backup does. This line is restic 0.19.1's.
	locked := `{"message_type":"exit_error","code":1,"message":"Fatal: unable to create lock in backend: repository is already locked by PID 7 on backup-2 by root (UID 0, GID 0)\nlock was created at 2026-10-01 07:12:44 (1m2s ago)"}`
	if got := failureMessage("open repository\n"+locked+"\n", repo); got != "Fatal: unable to create lock in backend: repository is already locked by PID 7 on backup-2 by root (UID 0, GID 0)" {
		t.Errorf("from restic's JSON: %q", got)
	}
	missing := `{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file: Stat: The specified key does not exist.\nIs there a repository at the following location?\n` + repo + `"}`
	if got := failureMessage(missing, repo); !strings.Contains(got, "repository does not exist") || strings.Contains(got, "ops-backups") || strings.Contains(got, "message_type") {
		t.Errorf("from restic's JSON: %q", got)
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
// to another, and retains deletion ownership until that target is reachable.
func TestManagerKnowsWhereABackupWent(t *testing.T) {
	st, err := store.Open(store.Config{Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "m.db"), Silent: true})
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
	m.ReadIgnore = noIgnoreFile
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
	if got, err := st.GetBackup(old.ID); err != nil || got.Phase != models.BackupDeleting || got.JobName != "" {
		t.Errorf("deletion lost ownership of the previous target's snapshot: %+v %v", got, err)
	}
}

// An unreachable target reached users as restic's own line: "Fatal: create
// repository ... dial tcp 10.96.39.25:9000: connect: connection refused",
// which says nothing of where to look, and names the object store's address,
// which a backup's message may not (anyone who can see the server reads it).
// Each cause is now said as what to check, with neither address nor bucket.
// The lines are restic 0.17.3's, against a real object store.
func TestFailureMessageSaysWhatToCheck(t *testing.T) {
	repo := "s3:http://10.96.39.25:9000/ops-backups/mc-a1b2"
	for _, c := range []struct{ line, want string }{
		{`Fatal: create repository at s3:http://10.96.39.25:9000/ops-backups/mc-a1b2 failed: Fatal: unable to open repository at s3:http://10.96.39.25:9000/ops-backups/mc-a1b2: client.BucketExists: Head "http://10.96.39.25:9000/ops-backups/": dial tcp 10.96.39.25:9000: connect: connection refused`,
			"refused the connection"},
		{`Fatal: unable to open config file: Stat: Get "http://10.96.39.25:9000/ops-backups/?location=": dial tcp 10.96.39.25:9000: i/o timeout`,
			"did not answer"},
		{`Fatal: unable to open config file: Stat: Get "http://s3.lan:9000/ops-backups/?location=": dial tcp: lookup s3.lan on 10.96.0.10:53: no such host`,
			"does not resolve"},
		{`Fatal: unable to open config file: Stat: Get "https://10.96.39.25:9000/ops-backups/?location=": http: server gave HTTP response to HTTPS client`,
			"plain HTTP"},
		{`Fatal: unable to open config file: Stat: Get "https://s3.lan/ops-backups/?location=": tls: failed to verify certificate: x509: certificate signed by unknown authority`,
			"TLS certificate"},
		{`Fatal: unable to open config file: Stat: The request signature we calculated does not match the signature you provided. Check your key and signing method.`,
			"refused the backup keys"},
		{`Fatal: create key in repository at s3:http://10.96.39.25:9000/ops-backups/mc-a1b2 failed: Stat: Access Denied`,
			"refused the backup keys"},
		{`Fatal: unable to open config file: Stat: The specified bucket does not exist.`,
			"bucket does not exist"},
		{`Fatal: wrong password or no key found`,
			"repository password"},
	} {
		got := failureMessage(c.line+"\nIs there a repository at the following location?\n"+repo+"\n", repo)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s\n  said %q, want %q", c.line, got, c.want)
		}
		if strings.Contains(got, "10.96") || strings.Contains(got, "ops-backups") || strings.Contains(got, "s3.lan") {
			t.Errorf("the message gives the object store away: %q", got)
		}
	}
}

// An error no cause matches keeps restic's line, and that line may name the
// object store by its URL and address rather than the repository's: those go
// too.
func TestFailureMessageHidesTheObjectStore(t *testing.T) {
	repo := "s3:http://10.96.39.25:9000/ops-backups/mc-a1b2"
	got := failureMessage(`Fatal: unable to open config file: Stat: Get "http://10.96.39.25:9000/ops-backups/?location=": read tcp 10.244.1.7:51820->10.96.39.25:9000: read: connection reset by peer`, repo)
	if strings.Contains(got, "10.96.39.25") || strings.Contains(got, "ops-backups") {
		t.Errorf("message = %q", got)
	}
	if !strings.Contains(got, "connection reset by peer") {
		t.Errorf("message = %q, want what went wrong kept", got)
	}
}

// Stopping a server and backing it up, by hand or in a schedule, is how a world
// is copied while nothing writes to it. The backup started at once, while the
// game was still saving the world on its way out; it now waits for the game's
// pod to be gone, and no longer than stopWait.
func TestABackupWaitsForAStoppingServer(t *testing.T) {
	st, err := store.Open(store.Config{Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "m.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.SaveBackupConfig(&models.BackupConfig{Endpoint: "s3.example", Bucket: "b", KeepLast: 3}, "ak", "sk", "rp"); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv := &models.Server{Slug: "mc", Namespace: "quetzal-srv-mc", DesiredState: models.StateStopped}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("server: %v", err)
	}
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: srv.Namespace, Labels: map[string]string{reconciler.ServerLabel: srv.Slug}}}
	data := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "data-1", Namespace: srv.Namespace, Labels: map[string]string{reconciler.DataLabel: srv.Slug}}}
	cs := fake.NewSimpleClientset(game, data)
	m := NewManager(st, cluster.New(st, cluster.Clients{Clientset: cs}))
	m.ReadIgnore = noIgnoreFile
	ctx := context.Background()

	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m.processPending(ctx)
	if got, _ := st.GetBackup(b.ID); got.Phase != models.BackupPending {
		t.Fatalf("with the game still going down: %s, want Pending", got.Phase)
	}
	if err := cs.CoreV1().Pods(srv.Namespace).Delete(ctx, game.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	m.processPending(ctx)
	if got, _ := st.GetBackup(b.ID); got.Phase != models.BackupRunning {
		t.Fatalf("once the game is gone: %s, want Running (the data manager does not count)", got.Phase)
	}

	// A pod that will not go does not hold a backup back for good.
	if _, err := cs.CoreV1().Pods(srv.Namespace).Create(ctx, game, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	stuck := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(stuck); err != nil {
		t.Fatalf("seed: %v", err)
	}
	done, _ := st.GetBackup(b.ID) // one operation per server at a time
	done.Phase = models.BackupSucceeded
	_ = st.UpdateBackup(done)
	m.Now = func() time.Time { return time.Now().Add(stopWait + time.Minute) }
	m.processPending(ctx)
	if got, _ := st.GetBackup(stuck.ID); got.Phase != models.BackupRunning {
		t.Errorf("past stopWait: %s, want Running", got.Phase)
	}

	// A running server's backup never waited, and does not now.
	srv.DesiredState = models.StateRunning
	if err := st.SetDesiredState(srv.ID, models.StateRunning); err != nil {
		t.Fatal(err)
	}
	m.Now = time.Now
	live := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(live); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stuckDone, _ := st.GetBackup(stuck.ID)
	stuckDone.Phase = models.BackupSucceeded
	_ = st.UpdateBackup(stuckDone)
	m.processPending(ctx)
	if got, _ := st.GetBackup(live.ID); got.Phase != models.BackupRunning {
		t.Errorf("a running server's backup: %s, want Running", got.Phase)
	}
}
