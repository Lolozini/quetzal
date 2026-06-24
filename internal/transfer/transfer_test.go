package transfer

import (
	"context"
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
