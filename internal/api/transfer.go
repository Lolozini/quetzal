package api

import (
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

type transferRequest struct {
	TargetCluster uint `json:"targetCluster"`
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
	var req transferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.TargetCluster == srv.ClusterID {
		writeError(w, http.StatusBadRequest, "server is already on that cluster")
		return
	}
	// The target must be a real, registered cluster.
	target, err := s.Store.GetCluster(req.TargetCluster)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown target cluster")
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
		TargetCluster: req.TargetCluster,
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
