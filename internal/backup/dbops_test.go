package backup

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/testdb"
)

const dbNS = "quetzal-srv-ts"

// dbFixture stores a stopped server holding the named databases on one
// external host, and returns a manager driving a fake cluster. withTarget
// configures backups.
func dbFixture(t *testing.T, withTarget bool, names ...string) (*Manager, *store.Store, *models.Server, []models.ServerDatabase, *fake.Clientset) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "db.db"), Silent: true,
		SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if withTarget {
		if err := st.SaveBackupConfig(&models.BackupConfig{Endpoint: "s3.example", Bucket: "b", KeepLast: 3}, "ak", "sk", "pw"); err != nil {
			t.Fatalf("backup config: %v", err)
		}
	}
	srv := &models.Server{Slug: "ts", Namespace: dbNS, DesiredState: models.StateStopped}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("server: %v", err)
	}
	host := &models.DatabaseHost{Name: "lan", Kind: models.DBHostExternal, Host: "admin.db.lan", Port: 3306,
		ConnectHost: "db.lan", ConnectPort: 3307, AdminUser: "root"}
	if err := st.CreateDatabaseHost(host, "rootpw"); err != nil {
		t.Fatalf("host: %v", err)
	}
	var dbs []models.ServerDatabase
	for _, n := range names {
		d := &models.ServerDatabase{ServerID: srv.ID, HostID: host.ID, DatabaseName: n, Username: "u_" + n, Remote: "%"}
		if err := st.CreateServerDatabase(d, "pw-"+n); err != nil {
			t.Fatalf("database: %v", err)
		}
		dbs = append(dbs, *d)
	}
	cs := fake.NewSimpleClientset()
	return NewManager(st, cluster.New(st, cluster.Clients{Clientset: cs})), st, srv, dbs, cs
}

func jobOf(t *testing.T, cs *fake.Clientset, name string) *batchv1.Job {
	t.Helper()
	job, err := cs.BatchV1().Jobs(dbNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job %s: %v", name, err)
	}
	return job
}

func secretOf(t *testing.T, cs *fake.Clientset) map[string]string {
	t.Helper()
	sec, err := cs.CoreV1().Secrets(dbNS).Get(context.Background(), CredsSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	return sec.StringData
}

// finishJob marks a Job done, or failed with the given container exiting in
// error in its pod.
func finishJob(t *testing.T, cs *fake.Clientset, name string, failedContainer string, initContainer bool) {
	t.Helper()
	ctx := context.Background()
	job := jobOf(t, cs, name)
	if failedContainer == "" {
		job.Status.Succeeded = 1
	} else {
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		st := corev1.ContainerStatus{Name: failedContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-x", Namespace: dbNS, Labels: map[string]string{"job-name": name}}}
		if initContainer {
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{st}
		} else {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{st}
		}
		if _, err := cs.CoreV1().Pods(dbNS).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cs.BatchV1().Jobs(dbNS).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A backup dumps every database of the server, reached at the address handed
// to servers with the server's own account, and records which it holds.
func TestABackupTakesTheServersDatabases(t *testing.T) {
	m, st, srv, dbs, cs := dbFixture(t, true, "s1_aaaa", "s1_bbbb")
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	m.Process(context.Background())
	got, _ := st.GetBackup(b.ID)
	if got.Phase != models.BackupRunning || strings.Join(got.Databases, ",") != "s1_aaaa,s1_bbbb" {
		t.Fatalf("backup = %s with databases %v, want Running with both", got.Phase, got.Databases)
	}
	spec := jobOf(t, cs, got.JobName).Spec.Template.Spec
	if len(spec.InitContainers) != 2 {
		t.Fatalf("init containers = %v", containerNames(spec.InitContainers))
	}
	env := envOf(spec.InitContainers[0])
	if env["DB_HOST"].Value != "db.lan" || env["DB_PORT"].Value != "3307" || env["DB_USER"].Value != dbs[0].Username {
		t.Errorf("the dump reaches %s:%s as %s, want the servers' address and the server's account", env["DB_HOST"].Value, env["DB_PORT"].Value, env["DB_USER"].Value)
	}
	if spec.InitContainers[0].Image != "mariadb:11.4" {
		t.Errorf("an external host is dumped with %s", spec.InitContainers[0].Image)
	}
	sec := secretOf(t, cs)
	if sec[dbPasswordKey(0)] != "pw-s1_aaaa" || sec[dbPasswordKey(1)] != "pw-s1_bbbb" || sec["RESTIC_PASSWORD"] != "pw" {
		t.Errorf("secret = %v", sec)
	}

	finishJob(t, cs, got.JobName, "", false)
	m.Process(context.Background())
	got, _ = st.GetBackup(b.ID)
	es, _ := st.ListEventsForServer(srv.ID, 0, 10)
	if got.Phase != models.BackupSucceeded || len(es) == 0 || !strings.Contains(es[0].Message, "with databases s1_aaaa, s1_bbbb") {
		t.Errorf("backup = %s, events %+v", got.Phase, es)
	}
}

// A database that cannot be dumped fails the backup, and the message is the
// database's -- not one of the object store's causes, which a "connection
// refused" from the database used to match.
func TestADumpThatFailsIsTheDatabasesFailure(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true, "s1_aaaa")
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	m.Process(context.Background())
	got, _ := st.GetBackup(b.ID)
	finishJob(t, cs, got.JobName, "dump-0", true)
	m.Process(context.Background())
	got, _ = st.GetBackup(b.ID)
	// The fake cluster answers every log request with "fake logs".
	if got.Phase != models.BackupFailed || got.Message != "fake logs" {
		t.Errorf("backup = %s %q, want Failed with the dump's own output", got.Phase, got.Message)
	}
}

// An import runs whether backups are configured or not -- it talks to the
// database alone, and its Secret holds that database's password and nothing
// else. It waits for the game to be gone, and calls itself off when the game
// will not go.
func TestADatabaseImportNeedsNoBackupTarget(t *testing.T) {
	m, st, srv, dbs, cs := dbFixture(t, false, "s1_aaaa")
	imp := &models.Backup{ServerID: srv.ID, Direction: models.DirDatabaseImport, Phase: models.BackupPending,
		DatabaseID: dbs[0].ID, Path: "/ts3.sql", Wipe: true}
	if err := st.CreateDatabaseImport(imp); err != nil {
		t.Fatal(err)
	}
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: dbNS, Labels: map[string]string{reconciler.ServerLabel: "ts"}}}
	if _, err := cs.CoreV1().Pods(dbNS).Create(context.Background(), game, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	clock := imp.CreatedAt.Add(time.Minute)
	m.Now = func() time.Time { return clock }
	m.Process(context.Background())
	if got, _ := st.GetBackup(imp.ID); got.Phase != models.BackupPending {
		t.Fatalf("with the game still up, the import is %s, want Pending", got.Phase)
	}

	_ = cs.CoreV1().Pods(dbNS).Delete(context.Background(), "server-1", metav1.DeleteOptions{})
	m.Process(context.Background())
	got, _ := st.GetBackup(imp.ID)
	if got.Phase != models.BackupRunning || got.JobName != "quetzal-db-import-"+strconv.FormatUint(uint64(imp.ID), 10) {
		t.Fatalf("import = %s (%s) %q, want Running", got.Phase, got.JobName, got.Message)
	}
	c := jobOf(t, cs, got.JobName).Spec.Template.Spec.Containers[0]
	if env := envOf(c); env["IMPORT_PATH"].Value != "/ts3.sql" || env["IMPORT_WIPE"].Value != "true" || env["DB_NAME"].Value != "s1_aaaa" {
		t.Errorf("import env = %+v", env)
	}
	if sec := secretOf(t, cs); len(sec) != 1 || sec[dbPasswordKey(0)] != "pw-s1_aaaa" {
		t.Errorf("an import's secret = %v", sec)
	}
	finishJob(t, cs, got.JobName, "", false)
	m.Process(context.Background())
	es, _ := st.ListEventsForServer(srv.ID, 0, 10)
	if got, _ := st.GetBackup(imp.ID); got.Phase != models.BackupSucceeded || len(es) == 0 ||
		es[0].Type != models.EventDatabaseImported || !strings.Contains(es[0].Message, "/ts3.sql loaded into database s1_aaaa") {
		t.Errorf("import = %s, events %+v", got.Phase, es)
	}
}

// An import whose game does not go is called off, and says so.
func TestAnImportThatCannotStartIsCalledOff(t *testing.T) {
	m, st, srv, dbs, cs := dbFixture(t, false, "s1_aaaa")
	imp := &models.Backup{ServerID: srv.ID, Direction: models.DirDatabaseImport, Phase: models.BackupPending, DatabaseID: dbs[0].ID, Path: "/x.sql"}
	if err := st.CreateDatabaseImport(imp); err != nil {
		t.Fatal(err)
	}
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: dbNS, Labels: map[string]string{reconciler.ServerLabel: "ts"}}}
	if _, err := cs.CoreV1().Pods(dbNS).Create(context.Background(), game, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return imp.CreatedAt.Add(restoreWait + time.Minute) }
	m.Process(context.Background())
	got, _ := st.GetBackup(imp.ID)
	if got.Phase != models.BackupFailed || !strings.Contains(got.Message, "nothing was imported") {
		t.Errorf("import = %s %q", got.Phase, got.Message)
	}
	// A database deleted since cannot be loaded.
	imp2 := &models.Backup{ServerID: srv.ID, Direction: models.DirDatabaseImport, Phase: models.BackupPending, DatabaseID: 999, Path: "/x.sql"}
	if err := st.CreateDatabaseImport(imp2); err != nil {
		t.Fatal(err)
	}
	_ = cs.CoreV1().Pods(dbNS).Delete(context.Background(), "server-1", metav1.DeleteOptions{})
	m.Process(context.Background())
	if got, _ := st.GetBackup(imp2.ID); got.Phase != models.BackupFailed || got.Message != "the database it was to load no longer exists" {
		t.Errorf("import of a gone database = %s %q", got.Phase, got.Message)
	}
}

// A restore asked for the databases loads back those of its snapshot the
// server still has, and says which it could not. A database made since the
// backup has no dump in it, and is left alone.
func TestARestoreLoadsTheDatabasesTheServerStillHas(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true, "s1_aaaa", "s1_new")
	// A backup holding two databases, recorded as a backup records them when
	// it starts; the server has since dropped one.
	src := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(src); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimBackup(src.ID, "j2", "", []string{"s1_aaaa", "s1_gone"}); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	src.Phase = models.BackupSucceeded
	if err := st.UpdateBackup(src); err != nil {
		t.Fatal(err)
	}
	r := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending, SourceID: src.ID, WithDatabases: true}
	if err := st.CreateRestore(r); err != nil {
		t.Fatal(err)
	}
	m.Process(context.Background())
	got, _ := st.GetBackup(r.ID)
	if got.Phase != models.BackupRunning || strings.Join(got.Databases, ",") != "s1_aaaa" {
		t.Fatalf("restore = %s loading %v, want Running loading s1_aaaa", got.Phase, got.Databases)
	}
	spec := jobOf(t, cs, got.JobName).Spec.Template.Spec
	if names := containerNames(spec.InitContainers); strings.Join(names, ",") != "restic,load-0" {
		t.Errorf("init containers = %v", names)
	}
	finishJob(t, cs, got.JobName, "", false)
	m.Process(context.Background())
	got, _ = st.GetBackup(r.ID)
	if got.Phase != models.BackupSucceeded || got.Message != "database s1_gone was not restored: the server no longer has it" {
		t.Errorf("restore = %s %q", got.Phase, got.Message)
	}
	es, _ := st.ListEventsForServer(srv.ID, 0, 10)
	if len(es) == 0 || !strings.Contains(es[0].Message, "with database s1_aaaa; database s1_gone was not restored") {
		t.Errorf("events = %+v", es)
	}

	// The files alone: no database is loaded.
	r2 := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending, SourceID: src.ID}
	if err := st.CreateRestore(r2); err != nil {
		t.Fatal(err)
	}
	m.Process(context.Background())
	got, _ = st.GetBackup(r2.ID)
	if spec := jobOf(t, cs, got.JobName).Spec.Template.Spec; len(spec.InitContainers) != 0 || len(got.Databases) != 0 {
		t.Errorf("a restore of the files alone loads %v with %v", got.Databases, containerNames(spec.InitContainers))
	}
}
