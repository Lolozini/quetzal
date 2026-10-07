// Package transfer migrates a server from one cluster to another. Kubernetes
// PVCs aren't portable across clusters, so data moves through the backup target
// (restic → S3, cluster-agnostic): the server is stopped and backed up on the
// source, its cluster is flipped, the snapshot is restored into a fresh volume
// on the destination, and finally the source namespace is deleted. The source
// data is left intact until the restore succeeds, so any failure rolls back
// cleanly. The state machine reuses the backup manager for the actual jobs.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/lolozini/quetzal/internal/backup"
	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// Manager advances in-progress cross-cluster transfers, one step per tick, in
// the leader controller.
type Manager struct {
	Store *store.Store
	// ClientsFor resolves a cluster's clients; defaults to the registry but is
	// overridable in tests.
	ClientsFor func(uint) (cluster.Clients, error)
	Now        func() time.Time
	// GetBackup reads a backup record. Injected for the same reason as the
	// clients: the difference between "this record is gone" and "the database
	// was busy" decides whether a transfer is torn down, and that is worth a
	// test rather than a comment.
	GetBackup func(uint) (*models.Backup, error)
}

// backup reads a backup record through the injected hook, or the store.
func (m *Manager) backup(id uint) (*models.Backup, error) {
	if m.GetBackup != nil {
		return m.GetBackup(id)
	}
	return m.Store.GetBackup(id)
}

// NewManager returns a transfer Manager backed by the cluster registry.
func NewManager(st *store.Store, reg *cluster.Registry) *Manager {
	return &Manager{Store: st, ClientsFor: reg.For, Now: time.Now}
}

// Process advances every server that has an active transfer.
func (m *Manager) Process(ctx context.Context) {
	srvs, err := m.Store.ListServersWithTransfer()
	if err != nil {
		log.Printf("transfer: list: %v", err)
		return
	}
	for i := range srvs {
		srv := &srvs[i]
		if srv.Transfer == nil {
			continue
		}
		// Cancellation first drains its linked operations on their original
		// clusters; placement never changes under an executable restore.
		if srv.Transfer.Cancelled {
			m.rollback(ctx, srv, srv.Transfer.Message)
			continue
		}
		switch srv.Transfer.Phase {
		case models.TransferBackingUp:
			m.advanceBackingUp(ctx, srv)
		case models.TransferRestoring:
			m.advanceRestoring(ctx, srv)
		case models.TransferCommitting:
			m.advanceCommitting(ctx, srv)
		}
	}
}

// advanceBackingUp waits for the source pod to be gone, takes a backup, then
// flips the server to the target cluster once the backup succeeds.
func (m *Manager) advanceBackingUp(ctx context.Context, srv *models.Server) {
	t := srv.Transfer
	if t.BackupID == 0 {
		cs, err := m.ClientsFor(t.SourceCluster)
		if err != nil {
			return // source unreachable; retry next tick
		}
		// Wait for a quiescent volume: no pod may still be writing to it.
		if has, err := hasServerPods(ctx, cs.Clientset, srv.Namespace, srv.Slug); err != nil || has {
			return
		}
		if err := m.Store.CreateTransferOperation(srv.ID, models.DirBackup); err != nil {
			log.Printf("transfer %s: create backup: %v", srv.Slug, err)
		}
		return
	}
	b, err := m.backup(t.BackupID)
	if errors.Is(err, store.ErrNotFound) {
		m.rollback(ctx, srv, "backup record lost")
		return
	}
	if err != nil {
		// Any other error is the database being momentarily unavailable, not a
		// verdict on the transfer. Cancelling on one would throw away a move that
		// was going fine because SQLite was busy for a moment.
		log.Printf("transfer %s: read backup (retrying next tick): %v", srv.Slug, err)
		return
	}
	switch b.Phase {
	case models.BackupSucceeded:
		// The data is safely in S3; hand the server to the target cluster. The
		// next reconcile creates the namespace + an empty volume there.
		if err := m.Store.AdvanceServerTransfer(srv.ID); err != nil {
			log.Printf("transfer %s: advance: %v", srv.Slug, err)
		}
	case models.BackupFailed:
		m.rollback(ctx, srv, "backup failed: "+b.Message)
	}
}

// advanceRestoring waits for the destination volume to exist, restores the
// snapshot into it, then deletes the source namespace and completes.
func (m *Manager) advanceRestoring(ctx context.Context, srv *models.Server) {
	t := srv.Transfer
	cs, err := m.ClientsFor(t.TargetCluster)
	if err != nil {
		return // target unreachable; retry
	}
	if t.RestoreID == 0 {
		ready, err := destinationReady(ctx, cs.Clientset, srv)
		if err != nil || !ready {
			return // wait for the reconciler to create the namespace/volume
		}
		if err := m.Store.CreateTransferOperation(srv.ID, models.DirRestore); err != nil {
			log.Printf("transfer %s: create restore: %v", srv.Slug, err)
		}
		return
	}
	r, err := m.backup(t.RestoreID)
	if errors.Is(err, store.ErrNotFound) {
		m.rollback(ctx, srv, "restore record lost")
		return
	}
	if err != nil {
		// Same as above, and it matters more here: rollback deletes the
		// destination namespace, so treating a transient read as a lost record
		// would destroy a restore that may well have completed.
		log.Printf("transfer %s: read restore (retrying next tick): %v", srv.Slug, err)
		return
	}
	switch r.Phase {
	case models.BackupSucceeded:
		if err := m.Store.CommitServerTransfer(srv.ID); err != nil {
			log.Printf("transfer %s: commit: %v", srv.Slug, err)
			return
		}
		m.advanceCommitting(ctx, srv)
	case models.BackupFailed:
		m.rollback(ctx, srv, "restore failed: "+r.Message)
	}
}

// advanceCommitting retries source cleanup after cancellation has been closed.
func (m *Manager) advanceCommitting(ctx context.Context, srv *models.Server) {
	t := srv.Transfer
	cs, err := m.ClientsFor(t.SourceCluster)
	if err != nil {
		return
	}
	if err := deleteNamespace(ctx, cs.Clientset, srv.Namespace); err != nil {
		log.Printf("transfer %s: delete source namespace: %v", srv.Slug, err)
		return
	}
	if err := m.Store.FinishServerTransfer(srv.ID, false); err != nil {
		log.Printf("transfer %s: finish: %v", srv.Slug, err)
		return
	}
	m.emit(srv, fmt.Sprintf("transfer to cluster %d complete", t.TargetCluster))
}

// rollback keeps the freeze until linked Jobs and pods have stopped. Pending
// operations are terminated atomically before any cluster placement changes.
func (m *Manager) rollback(ctx context.Context, srv *models.Server, msg string) {
	current, err := m.Store.CancelServerTransfer(srv.ID, msg)
	if err != nil {
		log.Printf("transfer %s: cancel operations: %v", srv.Slug, err)
		return
	}
	srv = current
	t := srv.Transfer
	for _, op := range []struct{ id, cluster uint }{{t.BackupID, t.SourceCluster}, {t.RestoreID, t.TargetCluster}} {
		if op.id == 0 {
			continue
		}
		stopped, err := m.stopOperation(ctx, srv, op.id, op.cluster)
		if err != nil {
			log.Printf("transfer %s: stop operation %d: %v", srv.Slug, op.id, err)
			return
		}
		if !stopped {
			return
		}
	}
	if t.Phase != models.TransferBackingUp {
		cs, err := m.ClientsFor(t.TargetCluster)
		if err != nil {
			return
		}
		if err := deleteNamespace(ctx, cs.Clientset, srv.Namespace); err != nil {
			log.Printf("transfer %s: delete target namespace: %v", srv.Slug, err)
			return
		}
	}
	if err := m.Store.FinishServerTransfer(srv.ID, true); err != nil {
		log.Printf("transfer %s: rollback: %v", srv.Slug, err)
		return
	}
	m.emit(srv, "transfer cancelled: "+msg)
}

func (m *Manager) stopOperation(ctx context.Context, srv *models.Server, id, clusterID uint) (bool, error) {
	b, err := m.backup(id)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if b.JobName == "" && b.Phase != models.BackupRunning {
		return true, nil
	}
	if b.ClusterID != nil {
		clusterID = *b.ClusterID
	}
	cs, err := m.ClientsFor(clusterID)
	if err != nil {
		return false, err
	}
	name := b.JobName
	if name == "" {
		name = backup.JobName(backup.Params{BackupID: b.ID, Direction: b.Direction})
	}
	foreground := metav1.DeletePropagationForeground
	err = cs.Clientset.BatchV1().Jobs(srv.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &foreground})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	_, err = cs.Clientset.BatchV1().Jobs(srv.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	// Deleting the Job is not enough: a terminating pod may still be writing.
	pods, err := cs.Clientset.CoreV1().Pods(srv.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		return false, err
	}
	if len(pods.Items) != 0 {
		return false, nil
	}
	err = cs.Clientset.CoreV1().Secrets(srv.Namespace).Delete(ctx, backup.CredsSecretName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if b.Phase == models.BackupRunning {
		if err := m.Store.FinishCancelledTransferOperation(srv.ID, b.ID); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (m *Manager) emit(srv *models.Server, msg string) {
	if err := m.Store.AddEvent(&models.Event{
		ServerID: srv.ID, Type: models.EventServerTransfer, Message: srv.Slug + ": " + msg,
	}); err != nil {
		log.Printf("transfer %s: event: %v", srv.Slug, err)
	}
}

// destinationReady reports whether the target cluster has the objects a restore
// needs: the data PVC bound and ready in the server's namespace.
func destinationReady(ctx context.Context, cs kubernetes.Interface, srv *models.Server) (bool, error) {
	_, err := cs.CoreV1().PersistentVolumeClaims(srv.Namespace).Get(ctx, reconciler.DataVolume, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func hasServerPods(ctx context.Context, cs kubernetes.Interface, ns, slug string) (bool, error) {
	for _, label := range []string{reconciler.ServerLabel, reconciler.DataLabel} {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: label + "=" + slug})
		if err != nil {
			return false, err
		}
		if len(pods.Items) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func deleteNamespace(ctx context.Context, cs kubernetes.Interface, ns string) error {
	err := cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("namespace deletion is still in progress")
}
