package backup

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	"github.com/lolozini/quetzal/internal/models"
)

func TestLostJobCreateResponseKeepsRestoreExclusive(t *testing.T) {
	m, st, b, cs := pendingRestore(t)
	cs.PrependReactor("create", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		job := a.(ktesting.CreateAction).GetObject().(*batchv1.Job)
		if err := cs.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), job, job.Namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, errors.New("response lost after persistence")
	})
	m.Process(context.Background())
	got, err := st.GetBackup(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != models.BackupRunning {
		t.Fatalf("live restore recorded as %s", got.Phase)
	}
	if active, err := st.HasActiveRestore(b.ServerID); err != nil || !active {
		t.Fatalf("restore exclusion released: active=%v err=%v", active, err)
	}
	if err := st.StartServer(b.ServerID, m.now()); err == nil {
		t.Fatal("game start allowed while restore Job exists")
	}
}

func TestUnacknowledgedJobSubmissionResumesAfterRestart(t *testing.T) {
	m, st, b, cs := pendingRestore(t)
	fail := true
	cs.PrependReactor("create", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, errors.New("transport unavailable")
		}
		return false, nil, nil
	})
	m.Process(context.Background())
	got, _ := st.GetBackup(b.ID)
	if got.Phase != models.BackupRunning {
		t.Fatalf("submission uncertainty became %s", got.Phase)
	}
	fail = false
	// A fresh manager has no in-memory copy of the Job it meant to submit.
	resumed := NewManager(st, m.Reg)
	resumed.Process(context.Background())
	got, _ = st.GetBackup(b.ID)
	jobs, err := cs.BatchV1().Jobs("quetzal-srv-mc").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 1 || got.Phase != models.BackupRunning {
		t.Fatalf("submission was not resumed: phase=%s jobs=%+v err=%v", got.Phase, jobs, err)
	}
}

func TestObservedRestoreJobIsNeverReplayedAfterDisappearance(t *testing.T) {
	m, st, b, cs := pendingRestore(t)
	ctx := context.Background()
	m.Process(ctx)
	b, _ = st.GetBackup(b.ID)
	job, err := cs.BatchV1().Jobs("quetzal-srv-mc").Get(ctx, b.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.UID = "observed-job"
	if _, err := cs.BatchV1().Jobs(job.Namespace).Update(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	m.Process(ctx)
	if err := cs.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	m.Process(ctx)
	got, _ := st.GetBackup(b.ID)
	if got.Phase != models.BackupRunning || got.Message == "" {
		t.Fatalf("missing evidence released exclusion without an actionable message: %+v", got)
	}
	jobs, _ := cs.BatchV1().Jobs(job.Namespace).List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatal("an observed destructive Job was replayed")
	}
}

func TestCompletedJobSurvivesTerminalStoreFailure(t *testing.T) {
	m, st, b, cs := pendingRestore(t)
	ctx := context.Background()
	m.Process(ctx)
	b, _ = st.GetBackup(b.ID)
	job, err := cs.BatchV1().Jobs("quetzal-srv-mc").Get(ctx, b.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Succeeded = 1
	if _, err := cs.BatchV1().Jobs(job.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	fail := true
	if err := st.DB().Callback().Update().Before("gorm:update").Register("test:terminal-failure", func(tx *gorm.DB) {
		if fail && tx.Statement.Table == "backups" {
			fail = false
			tx.AddError(errors.New("temporary store outage"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB().Callback().Update().Remove("test:terminal-failure") })
	m.Process(ctx)
	if _, err := cs.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("completion evidence removed before commit: %v", err)
	}
	m.Process(ctx)
	got, _ := st.GetBackup(b.ID)
	if got.Phase != models.BackupSucceeded {
		t.Fatalf("completed restore became %s: %s", got.Phase, got.Message)
	}
}

func TestForgottenSnapshotSurvivesRecordDeleteFailure(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true)
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupDeleting}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.processDeleting(ctx)
	b, _ = st.GetBackup(b.ID)
	finishJob(t, cs, b.JobName, "", false)
	fail := true
	if err := st.DB().Callback().Delete().Before("gorm:delete").Register("test:delete-failure", func(tx *gorm.DB) {
		if fail && tx.Statement.Table == "backups" {
			fail = false
			tx.AddError(errors.New("temporary store outage"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB().Callback().Delete().Remove("test:delete-failure") })
	m.processDeleting(ctx)
	got, err := st.GetBackup(b.ID)
	if err != nil || got.Phase != models.BackupDeleting || !got.Forgotten {
		t.Fatalf("forgotten result was not durable before cleanup: %+v %v", got, err)
	}
	m.processDeleting(ctx)
	if got, err := st.GetBackup(b.ID); err == nil {
		t.Fatalf("deleted snapshot still advertised: %+v", got)
	}
}

func TestForgetEvidenceSurvivesResultPersistenceFailure(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true)
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupDeleting}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.processDeleting(ctx)
	b, _ = st.GetBackup(b.ID)
	finishJob(t, cs, b.JobName, "", false)
	fail := true
	if err := st.DB().Callback().Update().Before("gorm:update").Register("test:forget-result", func(tx *gorm.DB) {
		if fail && tx.Statement.Table == "backups" {
			fail = false
			tx.AddError(errors.New("temporary store outage"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB().Callback().Update().Remove("test:forget-result") })
	m.processDeleting(ctx)
	if _, err := cs.BatchV1().Jobs(dbNS).Get(ctx, b.JobName, metav1.GetOptions{}); err != nil {
		t.Fatalf("forget evidence removed before durable result: %v", err)
	}
	m.processDeleting(ctx)
	if got, err := st.GetBackup(b.ID); err == nil {
		t.Fatalf("forgotten snapshot remains: %+v", got)
	}
}

func TestFailedForgetNeverAdvertisesPossiblyDeletedSnapshot(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true)
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupDeleting}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.processDeleting(ctx)
	b, _ = st.GetBackup(b.ID)
	// restic can remove the snapshot before its data-pruning stage fails.
	finishJob(t, cs, b.JobName, "restic", false)
	m.processDeleting(ctx)
	got, _ := st.GetBackup(b.ID)
	if got.Phase != models.BackupDeleting {
		t.Fatalf("uncertain snapshot advertised as %s", got.Phase)
	}
	m.processDeleting(ctx)
	got, _ = st.GetBackup(b.ID)
	if got.JobName == "" {
		t.Fatal("idempotent snapshot deletion was not retried")
	}
}

func TestRetentionPreservesOtherTargetsAndJobPolicy(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true)
	cfg, _ := st.GetBackupConfig()
	target := TargetID(cfg)
	other := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded, Target: "previous-target"}
	if err := st.CreateBackup(other); err != nil {
		t.Fatal(err)
	}
	var same []*models.Backup
	for range 2 {
		b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded, Target: target}
		if err := st.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
		same = append(same, b)
	}
	fresh := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(fresh); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.processPending(ctx)
	fresh, _ = st.GetBackup(fresh.ID)
	cfg.KeepLast = 1
	if err := st.SaveBackupConfig(cfg, "", "", ""); err != nil {
		t.Fatal(err)
	}
	finishJob(t, cs, fresh.JobName, "", false)
	m.processRunning(ctx)
	for _, b := range append(same, other) {
		got, err := st.GetBackup(b.ID)
		if err != nil || got.Phase != models.BackupSucceeded {
			t.Errorf("retention discarded record %d outside launched policy: %+v %v", b.ID, got, err)
		}
	}
}

func TestTerminalCleanupRetriesWithoutDeletingNewCredentials(t *testing.T) {
	m, st, b, cs := pendingRestore(t)
	ctx := context.Background()
	m.Process(ctx)
	b, _ = st.GetBackup(b.ID)
	job, err := cs.BatchV1().Jobs("quetzal-srv-mc").Get(ctx, b.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("uncommitted Job evidence expires automatically")
	}
	job.Status.Succeeded = 1
	if _, err := cs.BatchV1().Jobs(job.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	fail := true
	cs.PrependReactor("delete", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, errors.New("temporary apiserver outage")
		}
		return false, nil, nil
	})
	m.Process(ctx)
	got, _ := st.GetBackup(b.ID)
	if got.Phase != models.BackupSucceeded {
		t.Fatalf("cleanup changed durable outcome: %s", got.Phase)
	}
	sec, err := cs.CoreV1().Secrets(job.Namespace).Get(ctx, CredsSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sec.Labels[BackupLabel] = "next-operation"
	if _, err := cs.CoreV1().Secrets(job.Namespace).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	fail = false
	m.Process(ctx)
	if _, err := cs.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("terminal cleanup was never retried")
	}
	if _, err := cs.CoreV1().Secrets(job.Namespace).Get(ctx, CredsSecretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("cleanup removed another operation's credentials: %v", err)
	}
}
