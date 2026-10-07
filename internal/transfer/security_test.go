package transfer

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/lolozini/quetzal/internal/backup"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

func TestTransferWaitsForDataWriters(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns, Labels: map[string]string{reconciler.DataLabel: slug}}}
	h := newHarness(t, []runtime.Object{namespace(), pod}, nil)
	h.startTransfer(t, models.StateStopped)
	h.process()
	if s := h.reload(t); s.Transfer == nil || s.Transfer.BackupID != 0 {
		t.Fatalf("snapshot started while data writer exists: %+v", s.Transfer)
	}
	if err := h.src.CoreV1().Pods(ns).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.process()
	if s := h.reload(t); s.Transfer == nil || s.Transfer.BackupID == 0 {
		t.Fatalf("snapshot did not start after writers stopped: %+v", s.Transfer)
	}
}

func TestCancelTransferTerminatesPendingRestoreBeforeRollback(t *testing.T) {
	h := newHarness(t, []runtime.Object{namespace()}, []runtime.Object{namespace(), pvc()})
	h.startTransfer(t, models.StateStopped)
	h.process()
	b, err := h.st.GetBackup(h.reload(t).Transfer.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	b.Phase = models.BackupSucceeded
	if err := h.st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	h.process()
	h.process()
	s := h.reload(t)
	rid := s.Transfer.RestoreID
	if rid == 0 {
		t.Fatal("restore was not enqueued")
	}
	if _, err := h.st.CancelServerTransfer(s.ID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	h.process()
	s = h.reload(t)
	r, err := h.st.GetBackup(rid)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != models.BackupFailed {
		t.Fatalf("cancelled restore remains executable after rollback to cluster %d: %+v", s.ClusterID, r)
	}
	if s.ClusterID != srcCluster || s.Transfer != nil {
		t.Fatalf("rollback incomplete: %+v", s)
	}
	claimed, err := h.st.ClaimBackup(r.ID, "must-not-run-on-source", "", nil, "", "", "", srcCluster)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("backup manager can claim cancelled restore on source")
	}
}

func TestCancelTransferWaitsForRunningRestorePods(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "restore-live", Namespace: ns}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "restore-writer", Namespace: ns, Labels: map[string]string{"job-name": job.Name}}}
	h := newHarness(t, []runtime.Object{namespace()}, []runtime.Object{namespace(), pvc(), job, pod})
	h.startTransfer(t, models.StateStopped)
	h.process()
	b, err := h.st.GetBackup(h.reload(t).Transfer.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	b.Phase = models.BackupSucceeded
	if err := h.st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	h.process()
	h.process()
	s := h.reload(t)
	r, err := h.st.GetBackup(s.Transfer.RestoreID)
	if err != nil {
		t.Fatal(err)
	}
	r.Phase, r.JobName = models.BackupRunning, job.Name
	if err := h.st.UpdateBackup(r); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CancelServerTransfer(s.ID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	h.process()
	s = h.reload(t)
	if s.ClusterID != dstCluster || s.Transfer == nil {
		t.Fatalf("source reopened while target restore pod still writes: %+v", s)
	}
	r, err = h.st.GetBackup(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != models.BackupRunning {
		t.Fatalf("writer prematurely declared terminal: %s", r.Phase)
	}
	if err := h.dst.CoreV1().Pods(ns).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.process()
	s = h.reload(t)
	if s.ClusterID != srcCluster || s.Transfer != nil {
		t.Fatalf("rollback did not complete after writer exited: %+v", s)
	}
	r, err = h.st.GetBackup(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != models.BackupFailed {
		t.Fatalf("cancelled running restore remains executable: %s", r.Phase)
	}
}

func TestCancelTransferRetainsFreezeOnCleanupFailure(t *testing.T) {
	h := newHarness(t, []runtime.Object{namespace()}, []runtime.Object{namespace(), pvc()})
	h.startTransfer(t, models.StateStopped)
	h.process()
	b, err := h.st.GetBackup(h.reload(t).Transfer.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	b.Phase = models.BackupSucceeded
	if err := h.st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	h.process()
	s := h.reload(t)
	if _, err := h.st.CancelServerTransfer(s.ID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	h.dst.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("target unavailable")
	})
	h.process()
	s = h.reload(t)
	if s.ClusterID != dstCluster || s.Transfer == nil {
		t.Fatalf("cleanup failure silently abandoned transfer freeze: %+v", s)
	}
}

func TestTransferFreezeRefusesStoreStart(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.startTransfer(t, models.StateStopped)
	s := h.reload(t)
	if err := h.st.StartServer(s.ID, s.Transfer.StartedAt); err == nil {
		t.Fatal("transactional start bypassed transfer freeze")
	}
	if s := h.reload(t); s.DesiredState != models.StateStopped {
		t.Fatalf("frozen server became runnable: %s", s.DesiredState)
	}
}

func TestTransferCommitClosesCancellationBeforeSourceDeletion(t *testing.T) {
	h := newHarness(t, []runtime.Object{namespace()}, []runtime.Object{namespace(), pvc()})
	h.startTransfer(t, models.StateStopped)
	h.process()
	b, err := h.st.GetBackup(h.reload(t).Transfer.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if b.ClusterID == nil || *b.ClusterID != srcCluster {
		t.Fatal("snapshot cluster was not pinned")
	}
	b.Phase = models.BackupSucceeded
	if err := h.st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	h.process()
	h.process()
	r, err := h.st.GetBackup(h.reload(t).Transfer.RestoreID)
	if err != nil {
		t.Fatal(err)
	}
	if r.ClusterID == nil || *r.ClusterID != dstCluster {
		t.Fatal("restore cluster was not pinned")
	}
	r.Phase = models.BackupSucceeded
	if err := h.st.UpdateBackup(r); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	h.src.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		deletes++
		if _, err := h.st.CancelServerTransfer(h.srv.ID, "too late"); err == nil {
			t.Error("cancellation accepted after source cleanup began")
		}
		if deletes == 1 {
			return true, nil, errors.New("temporary source outage")
		}
		return false, nil, nil
	})
	h.process()
	if s := h.reload(t); s.Transfer == nil || s.Transfer.Phase != models.TransferCommitting {
		t.Fatalf("cleanup did not retain durable commit state: %+v", s.Transfer)
	}
	h.process()
	if s := h.reload(t); s.Transfer != nil || s.ClusterID != dstCluster {
		t.Fatalf("commit retry did not complete on target: %+v", s)
	}
	if _, err := h.dst.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
		t.Fatalf("destination deleted by late cancellation: %v", err)
	}
}

func TestTransferBackupSchedulesWithoutDataWriter(t *testing.T) {
	p := backup.Params{Namespace: ns, Slug: slug, BackupID: 1, Direction: models.DirBackup, Offline: true}
	job := backup.BuildJob(p)
	if job.Spec.Template.Spec.Affinity != nil {
		t.Fatal("offline transfer backup still requires the data pod that freeze removes")
	}
	p.Offline = false
	job = backup.BuildJob(p)
	if job.Spec.Template.Spec.Affinity == nil || job.Spec.Template.Spec.Affinity.PodAffinity == nil {
		t.Fatal("ordinary backup lost its live-volume scheduling affinity")
	}
}
