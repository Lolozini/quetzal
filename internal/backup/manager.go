package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	m.processDeleting(ctx)
}

// busyServers is the set of servers with an operation already holding their
// restic repository lock: a Running backup/restore, or a snapshot deletion whose
// Job is live. Nothing else for that server may start until it clears.
func (m *Manager) busyServers() map[uint]bool {
	busy := map[uint]bool{}
	if run, _ := m.Store.ListBackupsByPhase(models.BackupRunning); run != nil {
		for i := range run {
			busy[run[i].ServerID] = true
		}
	}
	if del, _ := m.Store.ListBackupsByPhase(models.BackupDeleting); del != nil {
		for i := range del {
			if del[i].JobName != "" {
				busy[del[i].ServerID] = true
			}
		}
	}
	return busy
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
	// A database import talks to the database alone, and runs whether backups
	// are configured or not; everything else needs the target.
	var access, secret, pass, unusable string
	cfg, err := m.Store.GetBackupConfig()
	if err != nil {
		unusable = "backups are not configured"
	} else if access, secret, pass, err = m.Store.BackupSecrets(cfg); err != nil {
		unusable = "decrypt backup credentials: " + err.Error()
	}
	// Serialize per server: never run two operations for the same server at once
	// (they would contend on that server's restic repository lock, or load a
	// database a backup is dumping).
	busy := m.busyServers()
	for i := range pend {
		b := &pend[i]
		imports := b.Direction == models.DirDatabaseImport
		if !imports && unusable != "" {
			m.finish(b, models.BackupFailed, 0, unusable)
			continue
		}
		if busy[b.ServerID] {
			continue
		}
		srv, err := m.Store.GetServer(b.ServerID)
		if err != nil {
			m.finish(b, models.BackupFailed, 0, "server not found")
			continue
		}
		// The API refuses to restore a backup made to another target, but the
		// target can change between the request and this tick.
		if b.Direction == models.DirRestore {
			if src, err := m.Store.GetBackup(b.SourceID); err == nil && src.Target != "" && src.Target != TargetID(cfg) {
				m.finish(b, models.BackupFailed, 0, OtherTargetMessage)
				continue
			}
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
			if err != nil {
				continue
			}
			if has {
				// The server cannot be started while a restore waits, but a pod
				// can still hold the volume: one stuck terminating, a game
				// started before that was refused. Such a restore is called off
				// rather than left to run whenever the pod goes -- days later,
				// over everything the game wrote meanwhile.
				if m.now().Sub(b.CreatedAt) > restoreWait {
					m.finish(b, models.BackupFailed, 0, fmt.Sprintf(
						"it never started: the server's data was still in use %s after the request, so nothing was restored", restoreWait))
				}
				continue
			}
		}
		// An import waits for the game to be gone: it would go on writing to
		// the database while the file replaces it. The server cannot be started
		// while an import waits, so this covers the stop's grace period, and a
		// pod that will not go calls the import off, as it does a restore.
		if imports {
			up, err := gameHasPods(ctx, cs, srv.Namespace, srv.Slug)
			if err != nil {
				continue
			}
			if up {
				if m.now().Sub(b.CreatedAt) > restoreWait {
					m.finish(b, models.BackupFailed, 0, fmt.Sprintf(
						"it never started: the server was still running %s after the request, so nothing was imported", restoreWait))
				}
				continue
			}
		}
		// A backup of a server on its way down waits for the game to be gone,
		// so that stopping a server and backing it up gives the copy of a
		// stopped server it was meant to: started at once, it copied the world
		// while the game was still saving it on the way out. Not forever: a pod
		// stuck terminating still gets its backup, taken live, after stopWait.
		if b.Direction == models.DirBackup && (srv.DesiredState != models.StateRunning || srv.Hibernated) && m.now().Sub(b.CreatedAt) < stopWait {
			up, err := gameHasPods(ctx, cs, srv.Namespace, srv.Slug)
			if err != nil || up {
				continue
			}
		}
		dbs, err := m.operationDatabases(srv, b)
		var stop opFailure
		if errors.As(err, &stop) {
			m.finish(b, models.BackupFailed, 0, stop.msg)
			continue
		}
		if err != nil {
			log.Printf("backup %d: %v (retrying)", b.ID, err)
			continue
		}
		p := Params{
			Namespace: srv.Namespace, Slug: srv.Slug,
			BackupID: b.ID, Direction: b.Direction, SourceID: b.SourceID,
			NodeSelector: srv.NodeSelector, InstallGen: srv.InstallGeneration,
			Databases: dbs, ImportPath: b.Path, ImportWipe: b.Wipe,
		}
		if !imports {
			p.Image, p.KeepLast, p.Repository, p.Region = Image(cfg), cfg.KeepLast, Repository(cfg, srv.Slug), cfg.Region
			p.AccessKey, p.SecretKey, p.RepoPassword = access, secret, pass
		}
		// Take the operation up before its Job exists: one cancelled meanwhile
		// is gone, and gets no Job.
		target := ""
		if b.Direction == models.DirBackup {
			target = TargetID(cfg)
		}
		var names []string
		if !imports && len(dbs) > 0 {
			names = DatabaseNames(dbs)
		}
		claimed, err := m.Store.ClaimBackup(b.ID, JobName(p), target, names)
		if err != nil {
			log.Printf("backup: claim %d: %v", b.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		b.Phase, b.JobName = models.BackupRunning, JobName(p)
		if target != "" {
			b.Target = target
		}
		if names != nil {
			b.Databases = names
		}
		busy[b.ServerID] = true
		if err := ensureSecret(ctx, cs, BuildSecret(p)); err != nil {
			m.finish(b, models.BackupFailed, 0, "create creds secret: "+err.Error())
			continue
		}
		job := BuildJob(p)
		if _, err := cs.BatchV1().Jobs(p.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			m.finish(b, models.BackupFailed, 0, "create job: "+err.Error())
			continue
		}
	}
}

func (m *Manager) processRunning(ctx context.Context) {
	run, err := m.Store.ListBackupsByPhase(models.BackupRunning)
	if err != nil || len(run) == 0 {
		return
	}
	keepLast := 7
	cfg, cfgErr := m.Store.GetBackupConfig()
	if cfgErr != nil || cfg == nil {
		cfg = &models.BackupConfig{} // redaction then matches nothing, as before
	} else if cfg.KeepLast > 0 {
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
			m.finish(b, models.BackupFailed, 0, string(b.Direction)+" job disappeared")
			continue
		}
		if err != nil {
			continue // transient; retry next tick
		}
		done, failed := jobOutcome(job)
		switch {
		case done:
			size := int64(0)
			if b.Direction == models.DirBackup {
				size = ParseBackupSize(podLogs(ctx, cs, srv.Namespace, b.JobName))
			}
			m.finish(b, models.BackupSucceeded, size, m.restoreNote(b))
			if b.Direction == models.DirBackup {
				if err := m.Store.PruneBackups(srv.ID, keepLast); err != nil {
					log.Printf("backup: prune %d: %v", srv.ID, err)
				}
			}
			cleanup(ctx, cs, srv.Namespace, b.JobName)
		case failed:
			step, logs := failedStep(ctx, cs, srv.Namespace, b.JobName)
			var msg string
			if isDatabaseContainer(step) {
				msg = databaseFailure(logs)
				log.Printf("backup: %s #%d of %s failed in %s: %s", b.Direction, b.ID, srv.Slug, step, msg)
			} else {
				log.Printf("backup: %s #%d of %s failed: %s", b.Direction, b.ID, srv.Slug, failureLine(logs, Repository(cfg, srv.Slug)))
				msg = failureMessage(logs, Repository(cfg, srv.Slug))
			}
			if msg == "" {
				msg = string(b.Direction) + " job failed"
			}
			m.finish(b, models.BackupFailed, 0, msg)
			cleanup(ctx, cs, srv.Namespace, b.JobName)
		}
	}
}

// opFailure ends an operation that cannot run as asked -- its database, or
// that database's host, is gone -- where any other error is waited out.
type opFailure struct{ msg string }

func (e opFailure) Error() string { return e.msg }

// operationDatabases resolves the databases an operation works on: every
// database of the server for a backup; for a restore asked for them, those of
// its snapshot the server still has (by name); for an import, the one it
// loads.
func (m *Manager) operationDatabases(srv *models.Server, b *models.Backup) ([]DatabaseParams, error) {
	var dbs []models.ServerDatabase
	switch {
	case b.Direction == models.DirBackup:
		all, err := m.Store.ListServerDatabases(srv.ID)
		if err != nil {
			return nil, fmt.Errorf("list the server's databases: %w", err)
		}
		dbs = all
	case b.Direction == models.DirRestore && b.WithDatabases:
		src, err := m.Store.GetBackup(b.SourceID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, opFailure{"the backup it restores no longer exists"}
		}
		if err != nil {
			return nil, fmt.Errorf("read the backup: %w", err)
		}
		all, err := m.Store.ListServerDatabases(srv.ID)
		if err != nil {
			return nil, fmt.Errorf("list the server's databases: %w", err)
		}
		for _, d := range all {
			if slices.Contains(src.Databases, d.DatabaseName) {
				dbs = append(dbs, d)
			}
		}
	case b.Direction == models.DirDatabaseImport:
		d, err := m.Store.GetServerDatabase(b.DatabaseID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && d.ServerID != srv.ID) {
			return nil, opFailure{"the database it was to load no longer exists"}
		}
		if err != nil {
			return nil, fmt.Errorf("read the database: %w", err)
		}
		dbs = []models.ServerDatabase{*d}
	}
	out := make([]DatabaseParams, 0, len(dbs))
	for i := range dbs {
		d := &dbs[i]
		host, err := m.Store.GetDatabaseHost(d.HostID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, opFailure{fmt.Sprintf("the host of database %s no longer exists", d.DatabaseName)}
		}
		if err != nil {
			return nil, fmt.Errorf("read the host of database %s: %w", d.DatabaseName, err)
		}
		pw, err := m.Store.ServerDatabasePassword(d)
		if err != nil {
			return nil, opFailure{fmt.Sprintf("the password of database %s cannot be read: %v", d.DatabaseName, err)}
		}
		// A managed host's own image: the admin's choice of registry and
		// release, already on the cluster. Any other host is reached with the
		// MariaDB release the panel deploys.
		image := reconciler.DefaultMariaDBImage
		if host.Kind == models.DBHostManaged && strings.TrimSpace(host.Image) != "" {
			image = host.Image
		}
		out = append(out, DatabaseParams{
			Name: d.DatabaseName, Host: host.ClientHost(), Port: host.ClientPort(),
			User: d.Username, Password: pw, Image: image,
		})
	}
	return out, nil
}

// restoreNote says which of its snapshot's databases a restore did not load
// back: those the server no longer has.
func (m *Manager) restoreNote(b *models.Backup) string {
	if b.Direction != models.DirRestore || !b.WithDatabases {
		return ""
	}
	src, err := m.Store.GetBackup(b.SourceID)
	if err != nil {
		return ""
	}
	var gone []string
	for _, name := range src.Databases {
		if !slices.Contains(b.Databases, name) {
			gone = append(gone, name)
		}
	}
	switch len(gone) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("database %s was not restored: the server no longer has it", gone[0])
	}
	return fmt.Sprintf("databases %s were not restored: the server no longer has them", strings.Join(gone, ", "))
}

// processDeleting drives snapshot deletions. A row in the Deleting phase has
// been dropped by the user but still owns a restic snapshot, so the record is
// kept until the forget Job confirms the data is gone from the repository — a
// failure must surface rather than leave the bucket holding data the user
// believes deleted. On success the row goes for good; on failure it returns to
// Succeeded carrying the reason, so the user can retry.
func (m *Manager) processDeleting(ctx context.Context) {
	del, err := m.Store.ListBackupsByPhase(models.BackupDeleting)
	if err != nil || len(del) == 0 {
		return
	}
	cfg, err := m.Store.GetBackupConfig()
	if errors.Is(err, store.ErrNotFound) {
		// No target configured any more: nothing can reach the snapshot, so the
		// record would sit in Deleting for good. Drop it and say so in the log.
		for i := range del {
			log.Printf("backup: dropping record %d without forgetting its snapshot (backups are not configured)", del[i].ID)
			_ = m.Store.DeleteBackup(del[i].ID)
		}
		return
	}
	if err != nil {
		log.Printf("backup: delete: read config: %v", err)
		return // transient; retry next tick rather than drop the rows
	}
	access, secret, pass, err := m.Store.BackupSecrets(cfg)
	if err != nil {
		log.Printf("backup: delete: decrypt credentials: %v", err)
		return
	}
	busy := m.busyServers()
	for i := range del {
		b := &del[i]
		srv, err := m.Store.GetServer(b.ServerID)
		if errors.Is(err, store.ErrNotFound) {
			// The server is gone; its namespace (and any Job we could run) went
			// with it, so there is nothing left to drive the delete.
			_ = m.Store.DeleteBackup(b.ID)
			continue
		}
		if err != nil {
			continue // transient store error; retry next tick rather than drop the row
		}
		// Its snapshot is in a target the panel no longer reaches: a forget run
		// against the current one would fail and keep the row forever.
		if b.Target != "" && b.Target != TargetID(cfg) && b.JobName == "" {
			log.Printf("backup: dropping record %d without forgetting its snapshot (made to a previous backup target)", b.ID)
			_ = m.Store.DeleteBackup(b.ID)
			continue
		}
		clients, err := m.Reg.For(srv.ClusterID)
		if err != nil {
			continue // cluster unreachable; retry next tick
		}
		cs := clients.Clientset
		p := Params{
			Image: Image(cfg), Namespace: srv.Namespace, Slug: srv.Slug,
			BackupID: b.ID, Direction: b.Direction, Forget: true,
			Repository: Repository(cfg, srv.Slug), Region: cfg.Region,
			AccessKey: access, SecretKey: secret, RepoPassword: pass,
		}
		if b.JobName == "" {
			if busy[b.ServerID] {
				continue // another operation holds the repository lock
			}
			if err := ensureSecret(ctx, cs, BuildSecret(p)); err != nil {
				log.Printf("backup: delete %d: creds secret: %v", b.ID, err)
				continue
			}
			job := BuildJob(p)
			if _, err := cs.BatchV1().Jobs(p.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
				m.failDelete(b, "create job: "+err.Error())
				continue
			}
			b.JobName = JobName(p)
			if err := m.Store.UpdateBackup(b); err != nil {
				log.Printf("backup: delete %d: update: %v", b.ID, err)
			}
			busy[b.ServerID] = true
			continue
		}
		job, err := cs.BatchV1().Jobs(srv.Namespace).Get(ctx, b.JobName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			m.failDelete(b, "the snapshot deletion job disappeared")
			continue
		}
		if err != nil {
			continue // transient; retry next tick
		}
		done, failed := jobOutcome(job)
		switch {
		case done:
			cleanup(ctx, cs, srv.Namespace, b.JobName)
			if err := m.Store.DeleteBackup(b.ID); err != nil {
				log.Printf("backup: delete %d: %v", b.ID, err)
			}
		case failed:
			logs := podLogs(ctx, cs, srv.Namespace, b.JobName)
			log.Printf("backup: removing #%d of %s failed: %s", b.ID, srv.Slug, failureLine(logs, Repository(cfg, srv.Slug)))
			msg := failureMessage(logs, Repository(cfg, srv.Slug))
			if msg == "" {
				msg = "the snapshot could not be removed"
			}
			cleanup(ctx, cs, srv.Namespace, b.JobName)
			m.failDelete(b, msg)
		}
	}
}

// jobOutcome reads whether a Job has finished, and how. A failed pod is not a
// failed Job: the Job retries (BackoffLimit), and it is only failed once it has
// given up, which Kubernetes records as its Failed condition. Reading the
// failure count instead called the operation failed on the first attempt and
// deleted the Job, so the retry it was built with never ran.
func jobOutcome(job *batchv1.Job) (done, failed bool) {
	if job.Status.Succeeded > 0 {
		return true, false
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false
		case batchv1.JobFailed:
			return false, true
		}
	}
	// No condition yet: a Job past its retries is failed even before the
	// controller writes the condition down.
	if limit := job.Spec.BackoffLimit; limit != nil && job.Status.Failed > *limit {
		return false, true
	}
	return false, false
}

// failDelete returns a record to Succeeded so it reappears in the list with the
// reason its snapshot could not be removed, rather than vanishing from the UI
// while the data stays in the bucket.
func (m *Manager) failDelete(b *models.Backup, msg string) {
	b.Phase = models.BackupSucceeded
	b.JobName = ""
	b.Message = "delete failed: " + msg
	if err := m.Store.UpdateBackup(b); err != nil {
		log.Printf("backup: delete %d: revert: %v", b.ID, err)
	}
}

// stopWait bounds how long a backup waits for a stopping server's game to go:
// well past any stop grace a template sets, short of leaving the backup to
// wait on a pod that will not go.
const stopWait = 15 * time.Minute

// restoreWait bounds how long a restore waits for its server's volume to be
// free. The pods of a stopped server are gone in seconds: past this, something
// holds the volume that will not let go on its own.
const restoreWait = 15 * time.Minute

// gameHasPods reports whether a server's game still has a pod, terminating or
// not. The data manager's does not count: it mounts the volume for the file
// manager, and writes nothing on its own.
func gameHasPods(ctx context.Context, cs kubernetes.Interface, ns, slug string) (bool, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: reconciler.ServerLabel + "=" + slug})
	if err != nil {
		return false, err
	}
	return len(pods.Items) > 0, nil
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
	// A Job may retry (BackoffLimit), leaving several pods in no particular
	// order. The outcome being reported belongs to the last attempt, so try that
	// one first — but a retry can fail before its container ever starts (a
	// missing secret, an unpullable image) and then has no logs at all. Falling
	// back through the earlier attempts is what surfaces the message that
	// actually explains the failure, instead of a bare "job failed".
	order := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		order = append(order, &pods.Items[i])
	}
	sort.SliceStable(order, func(i, j int) bool {
		return order[j].CreationTimestamp.Before(&order[i].CreationTimestamp)
	})
	for _, p := range order {
		data, err := cs.CoreV1().Pods(ns).GetLogs(p.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return string(data)
		}
	}
	// No attempt produced output. The reason a container never started is held
	// on the pod itself, and is usually the whole story.
	if reason := containerFailure(order[0]); reason != "" {
		return reason
	}
	return ""
}

// failedStep finds where a failed Job stopped: the container that failed in
// the last attempt that ran one, and what it printed. A pod with init
// containers stops at the first of them that fails -- a database that could
// not be dumped, before restic ever ran -- and its own logs say nothing of it.
// Without a failed container it falls back to the pod's logs (podLogs).
func failedStep(ctx context.Context, cs kubernetes.Interface, ns, jobName string) (container, logs string) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err == nil {
		order := make([]*corev1.Pod, 0, len(pods.Items))
		for i := range pods.Items {
			order = append(order, &pods.Items[i])
		}
		sort.SliceStable(order, func(i, j int) bool {
			return order[j].CreationTimestamp.Before(&order[i].CreationTimestamp)
		})
		for _, p := range order {
			name := failedContainer(p)
			if name == "" {
				continue
			}
			data, err := cs.CoreV1().Pods(ns).GetLogs(p.Name, &corev1.PodLogOptions{Container: name}).DoRaw(ctx)
			if err == nil && strings.TrimSpace(string(data)) != "" {
				return name, string(data)
			}
			if reason := containerFailure(p); reason != "" {
				return name, reason
			}
		}
	}
	return "", podLogs(ctx, cs, ns, jobName)
}

// failedContainer names the container of a pod that exited in error, init
// containers first: they run in order and the first to fail ends the pod.
func failedContainer(pod *corev1.Pod) string {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, st := range list {
			if t := st.State.Terminated; t != nil && t.ExitCode != 0 {
				return st.Name
			}
		}
	}
	return ""
}

// containerFailure returns why a pod's container did not run, if that is known:
// the waiting or terminated reason Kubernetes recorded. Empty when the container
// started normally (the failure is then in the logs, not here). Init containers
// come first: an image they cannot pull holds the whole pod.
func containerFailure(pod *corev1.Pod) string {
	for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		st := cs.State
		switch {
		case st.Waiting != nil && st.Waiting.Reason != "":
			return strings.TrimSpace(st.Waiting.Reason + ": " + st.Waiting.Message)
		case st.Terminated != nil && st.Terminated.Reason != "" && st.Terminated.ExitCode != 0:
			return strings.TrimSpace(st.Terminated.Reason + ": " + st.Terminated.Message)
		}
	}
	return strings.TrimSpace(pod.Status.Reason + " " + pod.Status.Message)
}

func (m *Manager) finish(b *models.Backup, phase models.BackupPhase, size int64, msg string) {
	now := m.now()
	b.Phase = phase
	b.SizeBytes = size
	b.Message = msg
	b.CompletedAt = &now
	if err := m.Store.UpdateBackup(b); err != nil {
		log.Printf("backup: finish %d: %v", b.ID, err)
		return
	}
	m.announce(b)
}

// announce records how an operation ended as an event, which the notification
// channels and the server's activity log both read. Nothing did: a scheduled
// backup that failed every night, on an expired key or a full bucket, failed
// in silence. An operation whose server is gone is not announced.
func (m *Manager) announce(b *models.Backup) {
	srv, err := m.Store.GetServer(b.ServerID)
	if err != nil {
		return
	}
	var typ, text string
	switch {
	case b.Direction == models.DirDatabaseImport:
		db := fmt.Sprintf("#%d", b.DatabaseID)
		if d, err := m.Store.GetServerDatabase(b.DatabaseID); err == nil {
			db = d.DatabaseName
		}
		if b.Phase == models.BackupSucceeded {
			typ, text = models.EventDatabaseImported, fmt.Sprintf("%s loaded into database %s", b.Path, db)
		} else {
			typ, text = models.EventDatabaseImportFailed, fmt.Sprintf("loading %s into database %s failed: %s", b.Path, db, b.Message)
		}
	case b.Direction == models.DirRestore && b.Phase == models.BackupSucceeded:
		typ, text = models.EventRestoreSucceeded, fmt.Sprintf("restored backup #%d", b.SourceID)
		if len(b.Databases) > 0 {
			text += ", with " + databaseList(b.Databases)
		}
		if b.Message != "" {
			text += "; " + b.Message
		}
	case b.Direction == models.DirRestore:
		typ, text = models.EventRestoreFailed, fmt.Sprintf("restoring backup #%d failed: %s", b.SourceID, b.Message)
	case b.Phase == models.BackupSucceeded:
		typ, text = models.EventBackupSucceeded, fmt.Sprintf("backup #%d completed", b.ID)
		if b.SizeBytes > 0 {
			text += fmt.Sprintf(" (%s)", byteSize(b.SizeBytes))
		}
		if len(b.Databases) > 0 {
			text += ", with " + databaseList(b.Databases)
		}
	default:
		typ, text = models.EventBackupFailed, fmt.Sprintf("backup #%d failed: %s", b.ID, b.Message)
	}
	if err := m.Store.AddEvent(&models.Event{ServerID: srv.ID, Type: typ, Message: srv.Slug + ": " + text}); err != nil {
		log.Printf("backup: announce %d: %v", b.ID, err)
	}
}

// databaseList names databases in a sentence: "database s4_x", "databases
// s4_x, s4_y".
func databaseList(names []string) string {
	if len(names) == 1 {
		return "database " + names[0]
	}
	return "databases " + strings.Join(names, ", ")
}

// byteSize writes a size the way the panel shows one: 1.4 GiB, 292 MiB.
func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v >= 100 {
		return fmt.Sprintf("%.0f %ciB", v, "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGTPE"[exp])
}

// redactRepository removes the restic repository URL from a message destined for
// the panel. restic names the repository in most of its errors, and a backup's
// message is readable by anyone with view access to the server — the weakest
// per-server permission — while the backup target itself is admin-only. On a
// panel with customers that would hand every one of them the operator's object
// store endpoint, bucket and prefix layout. What the message is actually for
// ("Access Denied", "no space left") survives the substitution.
func redactRepository(msg, repo string) string {
	if msg == "" || repo == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, repo, "the backup repository")
	// restic also prints the URL without its "s3:" scheme prefix in places.
	bare := strings.TrimPrefix(repo, "s3:")
	if bare != repo {
		msg = strings.ReplaceAll(msg, bare, "the backup repository")
	}
	// And an error about the bucket names the object store by its own URL and
	// address, which this missed: Get "http://10.96.39.25:9000/bucket/?location=":
	// dial tcp 10.96.39.25:9000: connection refused.
	if u, err := url.Parse(bare); err == nil && u.Host != "" {
		origin := u.Scheme + "://" + u.Host
		msg = regexp.MustCompile(regexp.QuoteMeta(origin)+`[^\s"]*`).ReplaceAllString(msg, "the object store")
		msg = strings.ReplaceAll(msg, u.Host, "the object store")
	}
	return msg
}

// failureMessage says why a restic run failed, for the backup's record: what
// to check when the cause is one restic's error only hints at, the error line
// itself, with the repository's location redacted, otherwise.
func failureMessage(logs, repo string) string {
	line := failureLine(logs, repo)
	if cause := failureCause(line); cause != "" {
		return cause
	}
	return redactRepository(line, repo)
}

// failureLine picks, from a failed restic run's output, the line that says what
// went wrong: the last "Fatal:" line when there is one, else the last line that
// is not restic pointing at the repository. restic ends a missing-repository
// error with "Is there a repository at the following location?" and the URL;
// redacted, the URL was all a failed restore used to say ("the backup
// repository").
func failureLine(logs, repo string) string {
	bare := strings.TrimPrefix(repo, "s3:")
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	fallback := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if msg, ok := exitError(l); ok {
			l = msg
		}
		if strings.HasPrefix(l, "Fatal:") {
			return l
		}
		pointer := l == repo || (repo != "" && l == bare) || strings.HasPrefix(l, "Is there a repository at the following location")
		if fallback == "" && l != "" && !pointer {
			fallback = l
		}
	}
	return fallback
}

// exitError reads the error restic prints as JSON when a command runs with
// --json, as the backup does, since 0.18:
// {"message_type":"exit_error","code":1,"message":"Fatal: ...\nIs there a ..."}.
// It returns the message's first line, the one that says what went wrong;
// without this the whole JSON line became the backup's message.
func exitError(line string) (string, bool) {
	if !strings.HasPrefix(line, "{") {
		return "", false
	}
	var e struct {
		Type    string `json:"message_type"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(line), &e) != nil || e.Type != "exit_error" {
		return "", false
	}
	first, _, _ := strings.Cut(e.Message, "\n")
	return strings.TrimSpace(first), true
}

// failureCauses turn a failed run's error into what to check. restic's own
// line names the object store's address and bucket, which a backup's message
// must not (it is readable by anyone who can see the server, the target by
// administrators only), and a refused connection or a timeout said nothing of
// where to look: an unreachable target reached users as "Fatal: create
// repository ... dial tcp ...: connection refused". The controller logs the
// line itself.
var failureCauses = []struct {
	signs []string
	cause string
}{
	{[]string{"connection refused"},
		"the object store refused the connection: it is not running, or the backup settings give the wrong address or port"},
	{[]string{"i/o timeout", "no route to host", "network is unreachable", "TLS handshake timeout"},
		"the object store did not answer: a firewall or a network policy between the cluster and it may drop the traffic, or the backup settings give the wrong address"},
	{[]string{"no such host", "server misbehaving"},
		"the object store's name does not resolve from the cluster: check the endpoint in the backup settings"},
	{[]string{"server gave HTTP response to HTTPS client", "does not look like a TLS handshake"},
		"the object store answers in plain HTTP while the backup settings ask for TLS"},
	{[]string{"x509:"},
		"the backup job does not trust the object store's TLS certificate"},
	{[]string{"signature we calculated does not match", "InvalidAccessKeyId", "Access Key Id you provided does not exist", "Access Denied", "AccessDenied"},
		"the object store refused the backup keys: check the access key and secret key in the backup settings"},
	{[]string{"bucket does not exist", "NoSuchBucket"},
		"the bucket does not exist: create it on the object store, or correct its name in the backup settings"},
	{[]string{"wrong password or no key found"},
		"the repository password does not open this server's backups: they were made with another one"},
}

func failureCause(line string) string {
	for _, c := range failureCauses {
		for _, sign := range c.signs {
			if strings.Contains(line, sign) {
				return c.cause
			}
		}
	}
	return ""
}
