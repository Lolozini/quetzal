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

// pendingRestore stores a stopped server with a backup to restore and a
// restore of it queued, and returns a manager driving a fake cluster that
// holds pods.
func pendingRestore(t *testing.T, pods ...*corev1.Pod) (*Manager, *store.Store, *models.Backup, *fake.Clientset) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "b.db"), Silent: true,
		SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.SaveBackupConfig(&models.BackupConfig{Endpoint: "s3.example", Bucket: "b", KeepLast: 3}, "ak", "sk", "pw"); err != nil {
		t.Fatalf("backup config: %v", err)
	}
	srv := &models.Server{Slug: "mc", Namespace: "quetzal-srv-mc", DesiredState: models.StateStopped}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("server: %v", err)
	}
	src := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded}
	if err := st.CreateBackup(src); err != nil {
		t.Fatalf("backup: %v", err)
	}
	restore := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending, SourceID: src.ID}
	if err := st.CreateRestore(restore); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restore, err = st.GetBackup(restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	cs := fake.NewSimpleClientset()
	for _, p := range pods {
		if _, err := cs.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return NewManager(st, cluster.New(st, cluster.Clients{Clientset: cs})), st, restore, cs
}

func gamePod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "server-abc", Namespace: "quetzal-srv-mc", Labels: map[string]string{reconciler.ServerLabel: "mc"},
	}}
}

// A restore waits for its server's volume to be free. It used to wait for good:
// the server, started under it, ran for days, and the restore rolled the world
// back the next time it stopped. One whose volume is still held after
// restoreWait is called off, and says so.
func TestARestoreThatCannotStartIsCalledOff(t *testing.T) {
	m, st, restore, cs := pendingRestore(t, gamePod())
	clock := restore.CreatedAt.Add(time.Minute)
	m.Now = func() time.Time { return clock }

	m.Process(context.Background())
	if b, _ := st.GetBackup(restore.ID); b.Phase != models.BackupPending {
		t.Fatalf("a minute in, the restore is %s, want still Pending", b.Phase)
	}

	clock = restore.CreatedAt.Add(restoreWait + time.Minute)
	m.Process(context.Background())
	b, err := st.GetBackup(restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Phase != models.BackupFailed || !strings.Contains(b.Message, "never started") {
		t.Errorf("restore = %s %q, want Failed saying it never started", b.Phase, b.Message)
	}
	jobs, _ := cs.BatchV1().Jobs("quetzal-srv-mc").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("a Job was created for a restore called off: %v", jobs.Items[0].Name)
	}
	es, _ := st.ListEventsForServer(b.ServerID, 0, 10)
	if len(es) == 0 || es[0].Type != models.EventRestoreFailed {
		t.Errorf("events = %+v, want restore.failed", es)
	}
}

// An operation is taken up before its Job is created, so that one cancelled
// meanwhile never gets a Job.
func TestAPendingOperationIsClaimedBeforeItsJob(t *testing.T) {
	m, st, restore, cs := pendingRestore(t)
	m.Process(context.Background())
	b, err := st.GetBackup(restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Phase != models.BackupRunning || b.JobName == "" {
		t.Fatalf("restore = %s job %q, want Running with its Job", b.Phase, b.JobName)
	}
	if _, err := cs.BatchV1().Jobs("quetzal-srv-mc").Get(context.Background(), b.JobName, metav1.GetOptions{}); err != nil {
		t.Errorf("the restore's Job: %v", err)
	}
	// Taken up, it can no longer be cancelled, nor taken up twice.
	if ok, _ := st.CancelPendingBackup(b.ID); ok {
		t.Error("a running restore was cancelled")
	}
	if ok, _ := st.ClaimBackup(b.ID, "again", "", nil); ok {
		t.Error("a running restore was taken up a second time")
	}
}
