package backup

import (
	"context"
	"log"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// Manager drives backup/restore operations to completion: it turns Pending rows
// into Jobs and finalizes Running rows from their Job status, on whichever
// cluster each server lives. It runs in the leader controller.
type Manager struct {
	Store *store.Store
	Reg   *cluster.Registry
	Now   func() time.Time
}

// NewManager returns a backup Manager.
func NewManager(st *store.Store, reg *cluster.Registry) *Manager {
	return &Manager{Store: st, Reg: reg, Now: time.Now}
}

// Process advances all in-flight operations one step.
func (m *Manager) Process(ctx context.Context) {
	m.processPending(ctx)
	m.processRunning(ctx)
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) processPending(ctx context.Context) {
	pend, err := m.Store.ListBackupsByPhase(models.BackupPending)
	if err != nil || len(pend) == 0 {
		return
	}
	cfg, err := m.Store.GetBackupConfig()
	if err != nil {
		for i := range pend {
			m.finish(&pend[i], models.BackupFailed, 0, "backups are not configured")
		}
		return
	}
	access, secret, pass, err := m.Store.BackupSecrets(cfg)
	if err != nil {
		for i := range pend {
			m.finish(&pend[i], models.BackupFailed, 0, "decrypt backup credentials: "+err.Error())
		}
		return
	}
	// Serialize per server: never run two operations for the same server at once
	// (they would contend on that server's restic repository lock).
	busy := map[uint]bool{}
	if run, _ := m.Store.ListBackupsByPhase(models.BackupRunning); run != nil {
		for i := range run {
			busy[run[i].ServerID] = true
		}
	}
	for i := range pend {
		b := &pend[i]
		if busy[b.ServerID] {
			continue
		}
		srv, err := m.Store.GetServer(b.ServerID)
		if err != nil {
			m.finish(b, models.BackupFailed, 0, "server not found")
			continue
		}
		clients, err := m.Reg.For(srv.ClusterID)
		if err != nil {
			log.Printf("backup %d: cluster unreachable, retrying: %v", b.ID, err)
			continue // leave Pending; retry next tick
		}
		cs := clients.Clientset
		// A restore overwrites the data volume in place. Never start it while a
		// pod still mounts that volume (the server must be stopped first): the
		// two read-write mounts would corrupt the data. Leave the op Pending and
		// retry once the pod has terminated. The API already refuses to enqueue a
		// restore for a running server; this also covers the stop grace period.
		if b.Direction == models.DirRestore {
			has, err := serverHasPods(ctx, cs, srv.Namespace, srv.Slug)
			if err != nil || has {
				continue
			}
		}
		p := Params{
			Image: Image(cfg), Namespace: srv.Namespace, Slug: srv.Slug,
			BackupID: b.ID, Direction: b.Direction, SourceID: b.SourceID,
			KeepLast: cfg.KeepLast, Repository: Repository(cfg, srv.Slug), Region: cfg.Region,
			AccessKey: access, SecretKey: secret, RepoPassword: pass,
			NodeSelector: srv.NodeSelector,
		}
		if err := ensureSecret(ctx, cs, BuildSecret(p)); err != nil {
			m.finish(b, models.BackupFailed, 0, "create creds secret: "+err.Error())
			continue
		}
		job := BuildJob(p)
		if _, err := cs.BatchV1().Jobs(p.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			m.finish(b, models.BackupFailed, 0, "create job: "+err.Error())
			continue
		}
		b.Phase = models.BackupRunning
		b.JobName = JobName(p)
		if err := m.Store.UpdateBackup(b); err != nil {
			log.Printf("backup: update %d: %v", b.ID, err)
		}
		busy[b.ServerID] = true
	}
}

func (m *Manager) processRunning(ctx context.Context) {
	run, err := m.Store.ListBackupsByPhase(models.BackupRunning)
	if err != nil || len(run) == 0 {
		return
	}
	keepLast := 7
	if cfg, err := m.Store.GetBackupConfig(); err == nil && cfg.KeepLast > 0 {
		keepLast = cfg.KeepLast
	}
	for i := range run {
		b := &run[i]
		srv, err := m.Store.GetServer(b.ServerID)
		if err != nil {
			m.finish(b, models.BackupFailed, 0, "server not found")
			continue
		}
		clients, err := m.Reg.For(srv.ClusterID)
		if err != nil {
			continue // cluster unreachable; retry next tick
		}
		cs := clients.Clientset
		job, err := cs.BatchV1().Jobs(srv.Namespace).Get(ctx, b.JobName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			m.finish(b, models.BackupFailed, 0, "backup job disappeared")
			continue
		}
		if err != nil {
			continue // transient; retry next tick
		}
		switch {
		case job.Status.Succeeded > 0:
			size := int64(0)
			if b.Direction == models.DirBackup {
				size = ParseBackupSize(podLogs(ctx, cs, srv.Namespace, b.JobName))
			}
			m.finish(b, models.BackupSucceeded, size, "")
			if b.Direction == models.DirBackup {
				if err := m.Store.PruneBackups(srv.ID, keepLast); err != nil {
					log.Printf("backup: prune %d: %v", srv.ID, err)
				}
			}
			cleanup(ctx, cs, srv.Namespace, b.JobName)
		case job.Status.Failed > 0:
			msg := lastLogLine(podLogs(ctx, cs, srv.Namespace, b.JobName))
			if msg == "" {
				msg = "backup job failed"
			}
			m.finish(b, models.BackupFailed, 0, msg)
			cleanup(ctx, cs, srv.Namespace, b.JobName)
		}
	}
}

// serverHasPods reports whether any pod that mounts the data volume still exists
// for a server — i.e. whether its data volume may still be mounted. That is the
// game pod (ServerLabel) or the data-manager pod (DataLabel); the activator never
// mounts data, so it is intentionally excluded. The reconciler scales the
// data-manager down while a restore is active, so this returns false once both
// are gone and the restore Job can take the volume exclusively.
func serverHasPods(ctx context.Context, cs kubernetes.Interface, ns, slug string) (bool, error) {
	for _, sel := range []string{
		reconciler.ServerLabel + "=" + slug,
		reconciler.DataLabel + "=" + slug,
	} {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return false, err
		}
		if len(pods.Items) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func ensureSecret(ctx context.Context, cs kubernetes.Interface, sec *corev1.Secret) error {
	_, err := cs.CoreV1().Secrets(sec.Namespace).Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, gerr := cs.CoreV1().Secrets(sec.Namespace).Get(ctx, sec.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		existing.Data = nil
		existing.StringData = sec.StringData
		_, uerr := cs.CoreV1().Secrets(sec.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
		return uerr
	}
	return err
}

func cleanup(ctx context.Context, cs kubernetes.Interface, ns, jobName string) {
	prop := metav1.DeletePropagationBackground
	_ = cs.BatchV1().Jobs(ns).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: &prop})
	_ = cs.CoreV1().Secrets(ns).Delete(ctx, CredsSecretName, metav1.DeleteOptions{})
}

func podLogs(ctx context.Context, cs kubernetes.Interface, ns, jobName string) string {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	data, err := cs.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		return ""
	}
	return string(data)
}

func (m *Manager) finish(b *models.Backup, phase models.BackupPhase, size int64, msg string) {
	now := m.now()
	b.Phase = phase
	b.SizeBytes = size
	b.Message = msg
	b.CompletedAt = &now
	if err := m.Store.UpdateBackup(b); err != nil {
		log.Printf("backup: finish %d: %v", b.ID, err)
	}
}

// lastLogLine returns the last non-empty line of a log blob (best-effort error
// message for a failed job).
func lastLogLine(logs string) string {
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}
