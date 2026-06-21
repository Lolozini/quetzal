package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/lolozini/quetzal/internal/models"
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
	if !s.requireAdmin(w, r) {
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
	if !s.requireAdmin(w, r) {
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
	if err := s.Store.SaveBackupConfig(cfg, req.AccessKey, req.SecretKey, req.RepoPassword); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	b, ok := s.lookupBackup(w, r, models.PermBackups)
	if !ok {
		return
	}
	if err := s.Store.DeleteBackup(b.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lookupBackup resolves {bid}, checks `perm` on the parent server, and that the
// backup belongs to it.
func (s *Server) lookupBackup(w http.ResponseWriter, r *http.Request, perm string) (*models.Backup, bool) {
	srv, ok := s.requireServer(w, r, perm)
	if !ok {
		return nil, false
	}
	bid, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("bid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid backup id")
		return nil, false
	}
	b, err := s.Store.GetBackup(uint(bid))
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
