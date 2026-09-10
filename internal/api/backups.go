package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/objectstore"
	"github.com/lolozini/quetzal/internal/store"
)

// ---- backup configuration (admin) ----

type backupConfigDTO struct {
	Endpoint       string `json:"endpoint"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	Region         string `json:"region"`
	UseSSL         bool   `json:"useSSL"`
	KeepLast       int    `json:"keepLast"`
	RunnerImage    string `json:"runnerImage"`
	Configured     bool   `json:"configured"`
	HasCredentials bool   `json:"hasCredentials"`
	HasPassword    bool   `json:"hasPassword"`
}

func (s *Server) handleGetBackupConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermSettings) {
		return
	}
	cfg, err := s.Store.GetBackupConfig()
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, backupConfigDTO{KeepLast: 7})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, backupConfigDTO{
		Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Prefix: cfg.Prefix, Region: cfg.Region,
		UseSSL: cfg.UseSSL, KeepLast: cfg.KeepLast, RunnerImage: cfg.RunnerImage,
		Configured:     cfg.Endpoint != "" && cfg.Bucket != "",
		HasCredentials: cfg.AccessKeyEnc != "" && cfg.SecretKeyEnc != "",
		HasPassword:    cfg.RepoPasswordEnc != "",
	})
}

type backupConfigRequest struct {
	Endpoint     string `json:"endpoint"`
	Bucket       string `json:"bucket"`
	Prefix       string `json:"prefix"`
	Region       string `json:"region"`
	UseSSL       bool   `json:"useSSL"`
	KeepLast     int    `json:"keepLast"`
	RunnerImage  string `json:"runnerImage"`
	AccessKey    string `json:"accessKey"`    // optional on update
	SecretKey    string `json:"secretKey"`    // optional on update
	RepoPassword string `json:"repoPassword"` // optional on update
}

func (s *Server) handleSetBackupConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermSettings) {
		return
	}
	var req backupConfigRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.Endpoint) == "" || strings.TrimSpace(req.Bucket) == "" {
		writeError(w, http.StatusBadRequest, "endpoint and bucket are required")
		return
	}
	existing, _ := s.Store.GetBackupConfig()
	firstTime := existing == nil
	// On first configuration the credentials + repo password are mandatory.
	if firstTime && (req.AccessKey == "" || req.SecretKey == "" || req.RepoPassword == "") {
		writeError(w, http.StatusBadRequest, "accessKey, secretKey and repoPassword are required on first setup")
		return
	}
	if req.KeepLast <= 0 {
		req.KeepLast = 7
	}
	cfg := &models.BackupConfig{
		Endpoint: req.Endpoint, Bucket: req.Bucket, Prefix: req.Prefix, Region: req.Region,
		UseSSL: req.UseSSL, KeepLast: req.KeepLast, RunnerImage: req.RunnerImage,
	}
	if err := s.checkBackupTarget(r.Context(), cfg, existing, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.SaveBackupConfig(cfg, req.AccessKey, req.SecretKey, req.RepoPassword); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkBackupTarget confirms the bucket is there and answers to the credentials,
// before they are stored.
//
// restic creates a bucket that does not exist, so a typo in the name never
// fails: it starts a second bucket and sends the backups there, and the mistake
// looks exactly like success until someone goes looking for the data. It also
// means a wrong key is only discovered by the first backup Job, hours later, in
// a log. Both are worth one request at the moment the target is configured.
//
// Being unable to reach the object store is not a rejection. The panel failing
// to connect does not mean the backup Jobs will, and refusing the configuration
// over a transient fault would leave the operator unable to set backups up at
// all — so an indeterminate result is logged and allowed through.
func (s *Server) checkBackupTarget(ctx context.Context, cfg *models.BackupConfig, existing *models.BackupConfig, req backupConfigRequest) error {
	access, secret := req.AccessKey, req.SecretKey
	if (access == "" || secret == "") && existing != nil {
		// An update that leaves the credentials blank keeps the stored ones.
		a, sec, _, err := s.Store.BackupSecrets(existing)
		if err != nil {
			return nil // cannot check without credentials; the save itself still works
		}
		if access == "" {
			access = a
		}
		if secret == "" {
			secret = sec
		}
	}
	if access == "" || secret == "" {
		return nil
	}
	if s.CheckBucket == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := s.CheckBucket(ctx, objectstore.Target{
		Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket,
		UseSSL: cfg.UseSSL, Access: access, Secret: secret,
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, objectstore.ErrIndeterminate) {
		log.Printf("backup target %s/%s saved without being verified: %v", cfg.Endpoint, cfg.Bucket, err)
		return nil
	}
	return err
}

// ---- per-server backups ----

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermView)
	if !ok {
		return
	}
	bs, err := s.Store.ListBackupsForServer(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermBackups)
	if !ok {
		return
	}
	if _, err := s.Store.GetBackupConfig(); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "backups are not configured")
		return
	}
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := s.Store.CreateBackup(b); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "backup.create", "")
	writeJSON(w, http.StatusAccepted, b)
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	src, ok := s.lookupBackup(w, r, models.PermBackups)
	if !ok {
		return
	}
	if src.Direction != models.DirBackup || src.Phase != models.BackupSucceeded {
		writeError(w, http.StatusBadRequest, "can only restore from a succeeded backup")
		return
	}
	// A restore overwrites the data volume in place. If the server is running, its
	// pod and the restore Job would mount the same volume read-write at the same
	// time (RWO allows this on a single node) and corrupt the data. Require a
	// stopped server first, mirroring Pterodactyl/Pelican.
	srv, err := s.Store.GetServer(src.ServerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if srv.DesiredState == models.StateRunning {
		writeError(w, http.StatusConflict, "stop the server before restoring (a live restore would corrupt the data volume)")
		return
	}
	b := &models.Backup{
		ServerID:  src.ServerID,
		Direction: models.DirRestore,
		Phase:     models.BackupPending,
		SourceID:  src.ID,
	}
	if err := s.Store.CreateBackup(b); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, src.ServerID, "backup.restore", "from #"+strconv.FormatUint(uint64(src.ID), 10))
	writeJSON(w, http.StatusAccepted, b)
}

// handleDeleteBackup removes a backup. A succeeded backup owns a restic snapshot,
// so it is not simply dropped from the database: it enters the Deleting phase and
// the controller forgets the snapshot from the repository first, otherwise the
// data would live on in the bucket after the user asked for it to be gone.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	b, ok := s.lookupBackup(w, r, models.PermBackups)
	if !ok {
		return
	}
	// An operation in flight owns a running Job — and, for a restore, the
	// exclusive write mount on the data volume (the reconciler keeps the data
	// manager scaled down while a restore row is Pending/Running). Dropping the
	// row would lift that guard mid-restore and orphan the Job, so refuse.
	switch b.Phase {
	case models.BackupPending, models.BackupRunning:
		writeError(w, http.StatusConflict, "this operation is still running; wait for it to finish before deleting it")
		return
	case models.BackupDeleting:
		w.WriteHeader(http.StatusNoContent) // already on its way out
		return
	}
	// Only a succeeded backup has a snapshot to forget; failed operations and
	// restore records are just history and can go straight away.
	if b.Direction == models.DirBackup && b.Phase == models.BackupSucceeded {
		if err := s.Store.MarkBackupDeleting(b.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.audit(r, b.ServerID, "backup.delete", "#"+strconv.FormatUint(uint64(b.ID), 10))
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.Store.DeleteBackup(b.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, b.ServerID, "backup.delete", "#"+strconv.FormatUint(uint64(b.ID), 10))
	w.WriteHeader(http.StatusNoContent)
}

// lookupBackup resolves {bid}, checks `perm` on the parent server, and that the
// backup belongs to it.
func (s *Server) lookupBackup(w http.ResponseWriter, r *http.Request, perm string) (*models.Backup, bool) {
	srv, ok := s.requireServer(w, r, perm)
	if !ok {
		return nil, false
	}
	bid, ok := pathID(r, "bid")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid backup id")
		return nil, false
	}
	b, err := s.Store.GetBackup(bid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "backup not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	if b.ServerID != srv.ID {
		writeError(w, http.StatusNotFound, "backup not found")
		return nil, false
	}
	return b, true
}
