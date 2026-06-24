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
	"fmt"
	"log"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

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
		switch srv.Transfer.Phase {
		case models.TransferBackingUp:
			m.advanceBackingUp(ctx, srv)
		case models.TransferRestoring:
			m.advanceRestoring(ctx, srv)
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
		b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
		if err := m.Store.CreateBackup(b); err != nil {
			log.Printf("transfer %s: create backup: %v", srv.Slug, err)
			return
		}
		t.BackupID = b.ID
		_ = m.Store.SetServerTransfer(srv.ID, t)
		return
	}
	b, err := m.Store.GetBackup(t.BackupID)
	if err != nil {
		m.abort(srv, "backup record lost")
		return
	}
	switch b.Phase {
	case models.BackupSucceeded:
		// The data is safely in S3; hand the server to the target cluster. The
		// next reconcile creates the namespace + an empty volume there.
		if err := m.Store.SetServerCluster(srv.ID, t.TargetCluster); err != nil {
			log.Printf("transfer %s: set cluster: %v", srv.Slug, err)
			return
		}
		t.Phase = models.TransferRestoring
		_ = m.Store.SetServerTransfer(srv.ID, t)
	case models.BackupFailed:
		m.abort(srv, "backup failed: "+b.Message)
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
		r := &models.Backup{
			ServerID: srv.ID, Direction: models.DirRestore,
			SourceID: t.BackupID, Phase: models.BackupPending,
		}
		if err := m.Store.CreateBackup(r); err != nil {
			log.Printf("transfer %s: create restore: %v", srv.Slug, err)
			return
		}
		t.RestoreID = r.ID
		_ = m.Store.SetServerTransfer(srv.ID, t)
		return
	}
	r, err := m.Store.GetBackup(t.RestoreID)
	if err != nil {
		m.rollback(ctx, srv, "restore record lost")
		return
	}
	switch r.Phase {
	case models.BackupSucceeded:
		// Tear down the source namespace; only complete once we've reached the
		// source to clean it up (otherwise the old copy would linger).
		scs, err := m.ClientsFor(t.SourceCluster)
		if err != nil {
			return
		}
		if err := deleteNamespace(ctx, scs.Clientset, srv.Namespace); err != nil {
			log.Printf("transfer %s: delete source namespace: %v", srv.Slug, err)
			return
		}
		prev := t.PrevState
		_ = m.Store.SetServerTransfer(srv.ID, nil)
		_ = m.Store.SetDesiredState(srv.ID, prev)
		m.emit(srv, fmt.Sprintf("transfer to cluster %d complete", t.TargetCluster))
	case models.BackupFailed:
		m.rollback(ctx, srv, "restore failed: "+r.Message)
	}
}

// abort cancels a transfer still in the backing-up phase: the server hasn't
// moved and its source data is intact, so restore the prior power state.
func (m *Manager) abort(srv *models.Server, msg string) {
	prev := srv.Transfer.PrevState
	_ = m.Store.SetServerTransfer(srv.ID, nil)
	_ = m.Store.SetDesiredState(srv.ID, prev)
	log.Printf("transfer %s aborted: %s", srv.Slug, msg)
	m.emit(srv, "transfer aborted: "+msg)
}

// rollback cancels a transfer after the cluster flip: the destination volume may
// be half-restored but the source data is untouched, so delete the destination
// namespace and move the server back to the source cluster.
func (m *Manager) rollback(ctx context.Context, srv *models.Server, msg string) {
	t := srv.Transfer
	if tcs, err := m.ClientsFor(t.TargetCluster); err == nil {
		_ = deleteNamespace(ctx, tcs.Clientset, srv.Namespace)
	}
	_ = m.Store.SetServerCluster(srv.ID, t.SourceCluster)
	prev := t.PrevState
	_ = m.Store.SetServerTransfer(srv.ID, nil)
	_ = m.Store.SetDesiredState(srv.ID, prev)
	log.Printf("transfer %s rolled back: %s", srv.Slug, msg)
	m.emit(srv, "transfer rolled back: "+msg)
}

func (m *Manager) emit(srv *models.Server, msg string) {
	_ = m.Store.AddEvent(&models.Event{
		ServerID: srv.ID, Type: models.EventServerTransfer, Message: srv.Slug + ": " + msg,
	})
}

// destinationReady reports whether the target cluster has the objects a restore
// needs: the data PVC (for PVC storage) or at least the namespace (hostPath).
func destinationReady(ctx context.Context, cs kubernetes.Interface, srv *models.Server) (bool, error) {
	if srv.Storage.Type == models.StoragePVC {
		_, err := cs.CoreV1().PersistentVolumeClaims(srv.Namespace).Get(ctx, reconciler.DataVolume, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	_, err := cs.CoreV1().Namespaces().Get(ctx, srv.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func hasServerPods(ctx context.Context, cs kubernetes.Interface, ns, slug string) (bool, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: reconciler.ServerLabel + "=" + slug,
	})
	if err != nil {
		return false, err
	}
	return len(pods.Items) > 0, nil
}

func deleteNamespace(ctx context.Context, cs kubernetes.Interface, ns string) error {
	err := cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
