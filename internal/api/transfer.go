package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// transferInProgress writes a 409 and returns true when the server is mid
// cross-cluster transfer (during which power/edit/reinstall are blocked to keep
// the migration consistent).
func transferInProgress(w http.ResponseWriter, srv *models.Server) bool {
	if srv.Transfer != nil {
		writeError(w, http.StatusConflict, "a cluster transfer is in progress for this server")
		return true
	}
	return false
}

// restoreActiveMessage answers what a waiting or running restore forbids.
const restoreActiveMessage = "a restore of this server's data is waiting or running; it has to finish, or be cancelled from the backups, first"

// restoreInProgress refuses, with a 409, what a restore waiting for the
// server's volume or writing it forbids: the data manager is down for it, and
// a transfer or an import would write the same volume.
func (s *Server) restoreInProgress(w http.ResponseWriter, srv *models.Server) bool {
	active, err := s.Store.HasActiveRestore(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return true
	}
	if active {
		writeError(w, http.StatusConflict, restoreActiveMessage)
		return true
	}
	return false
}

type transferRequest struct {
	// Cluster names the target by its slug, as creating a server does.
	Cluster string `json:"cluster"`
	// TargetCluster names it by its ID, the only form this endpoint took at
	// first. Still accepted.
	TargetCluster uint `json:"targetCluster"`
}

// transferTarget resolves the cluster a transfer request names. Creating a
// server on a cluster took its slug while moving one there took its ID, which a
// script had to look up first; either is accepted now, and both together if
// they agree.
func (s *Server) transferTarget(req transferRequest) (*models.Cluster, error) {
	slug := strings.TrimSpace(req.Cluster)
	switch {
	case slug != "":
		c, err := s.Store.GetClusterBySlug(slug)
		if err != nil {
			return nil, fmt.Errorf("unknown cluster %q", slug)
		}
		if req.TargetCluster != 0 && req.TargetCluster != c.ID {
			return nil, fmt.Errorf("cluster %q and targetCluster %d are two different clusters", slug, req.TargetCluster)
		}
		return c, nil
	case req.TargetCluster != 0:
		c, err := s.Store.GetCluster(req.TargetCluster)
		if err != nil {
			return nil, errors.New("unknown target cluster")
		}
		return c, nil
	}
	return nil, errors.New(`name the target cluster: "cluster", its slug`)
}

// handleTransferServer starts migrating a server to another cluster. It is an
// infrastructure action gated to server-admins: the server is stopped, its data
// is backed up on the source, then restored onto the target (see the transfer
// manager). Requires a configured backup target — that S3 repository is the data
// bridge between clusters.
// handleCancelTransfer marks an in-progress transfer for cancellation. The
// controller undoes it on its next tick — it is the side with cluster access,
// and keeping one implementation of "undo a transfer" beats a second one here
// that would have to delete namespaces of its own.
//
// This exists because a transfer that wedges — a restic Job stalled on an
// unreachable bucket, say — used to pin the server permanently: power, edits,
// suspension and backups all answer 409 while one is running, and the only way
// out was to delete the server.
func (s *Server) handleCancelTransfer(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermServers) {
		return
	}
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	if srv.Transfer == nil {
		writeError(w, http.StatusConflict, "no transfer is in progress")
		return
	}
	if srv.Transfer.Cancelled {
		writeJSON(w, http.StatusOK, srv.Transfer) // already asked; not an error
		return
	}
	t := *srv.Transfer
	t.Cancelled = true
	t.Message = "cancelling"
	if err := s.Store.SetServerTransfer(srv.ID, &t); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "server.transfer", "cancel requested")
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) handleTransferServer(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermServers) {
		return
	}
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	if srv.Transfer != nil {
		writeError(w, http.StatusConflict, "a transfer is already in progress")
		return
	}
	if importInProgress(w, srv) || s.restoreInProgress(w, srv) || s.databaseImportInProgress(w, srv) {
		return
	}
	var req transferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	// The target must be a real, registered cluster.
	target, err := s.transferTarget(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if target.ID == srv.ClusterID {
		writeError(w, http.StatusBadRequest, "server is already on that cluster")
		return
	}
	// Data crosses clusters via the backup target, so it must be configured.
	cfg, err := s.Store.GetBackupConfig()
	if err != nil || strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.Bucket) == "" {
		writeError(w, http.StatusBadRequest, "configure a backup target first (transfers move data through it)")
		return
	}

	// Stop the server (for a quiescent snapshot) and record the state to restore
	// once the move completes.
	t := &models.TransferState{
		Phase:         models.TransferBackingUp,
		SourceCluster: srv.ClusterID,
		TargetCluster: target.ID,
		PrevState:     srv.DesiredState,
		StartedAt:     time.Now(),
	}
	if err := s.Store.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.SetServerTransfer(srv.ID, t); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "server.transfer", "to cluster "+target.Slug)
	writeJSON(w, http.StatusAccepted, map[string]any{"result": "transfer started", "transfer": t})
}
