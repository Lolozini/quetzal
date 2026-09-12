package transfer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

const (
	srcCluster = uint(1)
	dstCluster = uint(2)
	ns         = "quetzal-srv-s1"
	slug       = "s1"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "t.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// harness wires a transfer manager to two fake clusters.
type harness struct {
	st  *store.Store
	m   *Manager
	src *fake.Clientset
	dst *fake.Clientset
	srv *models.Server
}

func newHarness(t *testing.T, srcObjs, dstObjs []runtime.Object) *harness {
	t.Helper()
	st := testStore(t)
	srv := &models.Server{
		Slug: slug, Namespace: ns, ClusterID: srcCluster,
		DesiredState: models.StateStopped,
		Storage:      models.Storage{Type: models.StoragePVC},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	src := fake.NewSimpleClientset(srcObjs...)
	dst := fake.NewSimpleClientset(dstObjs...)
	m := NewManager(st, nil)
	m.ClientsFor = func(id uint) (cluster.Clients, error) {
		switch id {
		case srcCluster:
			return cluster.Clients{Clientset: src}, nil
		case dstCluster:
			return cluster.Clients{Clientset: dst}, nil
		}
		return cluster.Clients{}, fmt.Errorf("unknown cluster %d", id)
	}
	return &harness{st: st, m: m, src: src, dst: dst, srv: srv}
}

// startTransfer mimics the API: stop + set BackingUp.
func (h *harness) startTransfer(t *testing.T, prev models.DesiredState) {
	t.Helper()
	_ = h.st.SetDesiredState(h.srv.ID, models.StateStopped)
	ts := &models.TransferState{
		Phase: models.TransferBackingUp, SourceCluster: srcCluster, TargetCluster: dstCluster,
		PrevState: prev, StartedAt: time.Now(),
	}
	if err := h.st.SetServerTransfer(h.srv.ID, ts); err != nil {
		t.Fatalf("set transfer: %v", err)
	}
}

func (h *harness) process() { h.m.Process(context.Background()) }

func (h *harness) reload(t *testing.T) *models.Server {
	t.Helper()
	s, err := h.st.GetServer(h.srv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return s
}

func pvc() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: reconciler.DataVolume, Namespace: ns}}
}
func namespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
}
func serverPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "p1", Namespace: ns, Labels: map[string]string{reconciler.ServerLabel: slug},
	}}
}

func TestTransferHappyPath(t *testing.T) {
	// Source has its namespace (to delete at the end); destination has the
	// reconciler-created PVC ready for the restore.
	h := newHarness(t, []runtime.Object{namespace()}, []runtime.Object{pvc()})
	h.startTransfer(t, models.StateRunning)

	// 1) BackingUp, no source pod → a backup is enqueued.
	h.process()
	s := h.reload(t)
	if s.Transfer == nil || s.Transfer.BackupID == 0 {
		t.Fatalf("expected a backup to be enqueued, transfer=%+v", s.Transfer)
	}
	bid := s.Transfer.BackupID

	// Simulate the backup manager finishing the backup.
	b, _ := h.st.GetBackup(bid)
	b.Phase = models.BackupSucceeded
	_ = h.st.UpdateBackup(b)

	// 2) Backup succeeded → cluster flips, phase → Restoring.
	h.process()
	s = h.reload(t)
	if s.ClusterID != dstCluster {
		t.Fatalf("cluster not flipped: %d", s.ClusterID)
	}
	if s.Transfer == nil || s.Transfer.Phase != models.TransferRestoring {
		t.Fatalf("phase = %+v, want Restoring", s.Transfer)
	}

	// 3) Restoring, destination PVC exists → a restore is enqueued.
	h.process()
	s = h.reload(t)
	if s.Transfer == nil || s.Transfer.RestoreID == 0 {
		t.Fatalf("expected a restore to be enqueued, transfer=%+v", s.Transfer)
	}
	r, _ := h.st.GetBackup(s.Transfer.RestoreID)
	if r.Direction != models.DirRestore || r.SourceID != bid {
		t.Fatalf("restore op wrong: %+v", r)
	}
	r.Phase = models.BackupSucceeded
	_ = h.st.UpdateBackup(r)

	// 4) Restore succeeded → source namespace deleted, transfer cleared, power
	// state restored.
	h.process()
	s = h.reload(t)
	if s.Transfer != nil {
		t.Fatalf("transfer not cleared: %+v", s.Transfer)
	}
	if s.DesiredState != models.StateRunning {
		t.Errorf("desired = %q, want Running (prev state restored)", s.DesiredState)
	}
	if _, err := h.src.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("source namespace should be deleted, err=%v", err)
	}
}

func TestTransferWaitsForQuiescentPod(t *testing.T) {
	// A pod still runs on the source → no backup should be enqueued yet.
	h := newHarness(t, []runtime.Object{namespace(), serverPod()}, nil)
	h.startTransfer(t, models.StateRunning)
	h.process()
	if s := h.reload(t); s.Transfer == nil || s.Transfer.BackupID != 0 {
		t.Fatalf("backup enqueued while a pod still mounts the volume: %+v", s.Transfer)
	}
}

func TestTransferAbortsOnBackupFailure(t *testing.T) {
	h := newHarness(t, []runtime.Object{namespace()}, nil)
	h.startTransfer(t, models.StateRunning)
	h.process() // enqueue backup
	bid := h.reload(t).Transfer.BackupID
	b, _ := h.st.GetBackup(bid)
	b.Phase = models.BackupFailed
	b.Message = "repo locked"
	_ = h.st.UpdateBackup(b)

	h.process() // should abort
	s := h.reload(t)
	if s.Transfer != nil {
		t.Errorf("transfer not cleared on abort: %+v", s.Transfer)
	}
	if s.ClusterID != srcCluster {
		t.Errorf("cluster changed on abort: %d", s.ClusterID)
	}
	if s.DesiredState != models.StateRunning {
		t.Errorf("desired = %q, want Running restored", s.DesiredState)
	}
}

func TestTransferRollsBackOnRestoreFailure(t *testing.T) {
	h := newHarness(t, []runtime.Object{namespace()}, []runtime.Object{pvc(), namespace()})
	h.startTransfer(t, models.StateStopped)
	h.process() // enqueue backup
	bid := h.reload(t).Transfer.BackupID
	b, _ := h.st.GetBackup(bid)
	b.Phase = models.BackupSucceeded
	_ = h.st.UpdateBackup(b)
	h.process() // flip cluster → Restoring
	h.process() // enqueue restore
	rid := h.reload(t).Transfer.RestoreID
	r, _ := h.st.GetBackup(rid)
	r.Phase = models.BackupFailed
	r.Message = "out of space"
	_ = h.st.UpdateBackup(r)

	h.process() // should roll back
	s := h.reload(t)
	if s.Transfer != nil {
		t.Errorf("transfer not cleared on rollback: %+v", s.Transfer)
	}
	if s.ClusterID != srcCluster {
		t.Errorf("cluster not rolled back to source: %d", s.ClusterID)
	}
	// Destination namespace should have been torn down.
	if _, err := h.dst.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("destination namespace should be deleted on rollback, err=%v", err)
	}
}

// A transfer used to be torn down by any error reading its backup record, not
// just a missing one. The restore phase is where that hurts: rollback deletes
// the destination namespace, so a moment of database contention would destroy a
// restore that may well have completed, on a move that was going fine.
func TestTransientReadDoesNotTearDownATransfer(t *testing.T) {
	h := newHarness(t, []runtime.Object{namespace()}, []runtime.Object{pvc(), namespace()})
	h.startTransfer(t, models.StateRunning)

	// Reach the restoring phase: back up, succeed, flip, create the restore.
	h.process()
	srv := h.reload(t)
	b, _ := h.st.GetBackup(srv.Transfer.BackupID)
	b.Phase = models.BackupSucceeded
	if err := h.st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	h.process() // -> Restoring
	h.process() // creates the restore record
	srv = h.reload(t)
	if srv.Transfer == nil || srv.Transfer.Phase != models.TransferRestoring || srv.Transfer.RestoreID == 0 {
		t.Fatalf("precondition: transfer = %+v", srv.Transfer)
	}

	// Now the database stumbles.
	h.m.GetBackup = func(uint) (*models.Backup, error) {
		return nil, errors.New("database is locked")
	}
	h.process()

	srv = h.reload(t)
	if srv.Transfer == nil {
		t.Fatal("a transient read error tore the transfer down")
	}
	if srv.Transfer.Phase != models.TransferRestoring {
		t.Errorf("phase = %q, want it untouched", srv.Transfer.Phase)
	}
	if srv.ClusterID != dstCluster {
		t.Errorf("the server was moved back to the source on a transient error")
	}
	// And the destination namespace, which rollback deletes, is still there.
	if _, err := h.dst.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
		t.Errorf("the destination namespace was deleted: %v", err)
	}

	// A record that is genuinely gone still rolls back.
	h.m.GetBackup = func(uint) (*models.Backup, error) { return nil, store.ErrNotFound }
	h.process()
	srv = h.reload(t)
	if srv.Transfer != nil {
		t.Errorf("a lost record should still roll the transfer back, got %+v", srv.Transfer)
	}
	if srv.ClusterID != srcCluster {
		t.Errorf("rollback should return the server to the source cluster, got %d", srv.ClusterID)
	}
}

// A wedged transfer used to pin its server for good: power, edits, suspension
// and backups all answer 409 while one is running, and nothing could end it
// short of deleting the server. Cancelling is honoured by the controller, and
// what it does depends on how far the move got.
func TestCancelBeforeTheFlipJustDropsTheTransfer(t *testing.T) {
	h := newHarness(t, []runtime.Object{namespace(), serverPod()}, nil)
	h.startTransfer(t, models.StateRunning)

	srv := h.reload(t)
	cancelled := *srv.Transfer
	cancelled.Cancelled = true
	if err := h.st.SetServerTransfer(srv.ID, &cancelled); err != nil {
		t.Fatal(err)
	}
	h.process()

	srv = h.reload(t)
	if srv.Transfer != nil {
		t.Errorf("transfer should be gone, got %+v", srv.Transfer)
	}
	if srv.ClusterID != srcCluster {
		t.Errorf("the server never left the source: cluster = %d", srv.ClusterID)
	}
	if srv.DesiredState != models.StateRunning {
		t.Errorf("desired state = %q, want the state from before the transfer", srv.DesiredState)
	}
	// Nothing was torn down: the source namespace is the live one.
	if _, err := h.src.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
		t.Errorf("the source namespace was deleted on a cancel that had moved nothing: %v", err)
	}
}

// After the cluster flip a cancel has to undo the move, not just forget it.
func TestCancelAfterTheFlipRollsBack(t *testing.T) {
	h := newHarness(t, []runtime.Object{namespace()}, []runtime.Object{pvc(), namespace()})
	h.startTransfer(t, models.StateRunning)
	h.process()
	srv := h.reload(t)
	b, _ := h.st.GetBackup(srv.Transfer.BackupID)
	b.Phase = models.BackupSucceeded
	if err := h.st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	h.process() // flips to the destination, phase -> Restoring
	srv = h.reload(t)
	if srv.ClusterID != dstCluster {
		t.Fatalf("precondition: the flip did not happen")
	}

	cancelled := *srv.Transfer
	cancelled.Cancelled = true
	if err := h.st.SetServerTransfer(srv.ID, &cancelled); err != nil {
		t.Fatal(err)
	}
	h.process()

	srv = h.reload(t)
	if srv.Transfer != nil {
		t.Errorf("transfer should be gone, got %+v", srv.Transfer)
	}
	if srv.ClusterID != srcCluster {
		t.Errorf("cluster = %d, want the server back on the source", srv.ClusterID)
	}
	// The half-built destination is cleaned up; the source, which holds the data,
	// is not.
	if _, err := h.dst.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the destination namespace survived the rollback: %v", err)
	}
	if _, err := h.src.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
		t.Errorf("the source namespace was deleted: %v", err)
	}
}
