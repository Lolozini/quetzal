package api

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// databaseImportActiveMessage answers what a waiting or running import forbids.
const databaseImportActiveMessage = "a database import of this server is waiting or running; it has to finish, or be cancelled from the databases, first"

// handleImportDatabase loads an SQL file of the server's into one of its
// databases: the dump of the database a game used elsewhere -- TeamSpeak's,
// a plugin's -- uploaded with the files. It took kubectl and a MySQL client of
// one's own. The controller runs it as a Job, with the server's own account,
// on a stopped server that cannot start until it is done (as a restore).
func (s *Server) handleImportDatabase(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermDatabases)
	if !ok {
		return
	}
	// The file is read from the server's files, and what it holds ends up in a
	// database the caller can read: a subuser trusted with the databases alone
	// would read any file of the server's through it.
	if !s.can(userFrom(r.Context()), srv, models.PermFiles) {
		writeError(w, http.StatusForbidden, "an import reads one of the server's files: it takes the files permission as well as the databases one")
		return
	}
	if transferInProgress(w, srv) {
		return
	}
	d, ok := s.lookupServerDatabase(w, r, srv.ID)
	if !ok {
		return
	}
	var req struct {
		// Path is the file, as the file manager names it (from the server's
		// data directory).
		Path string `json:"path"`
		// Wipe empties the database first (the default).
		Wipe *bool `json:"wipe"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	p := strings.TrimSpace(req.Path)
	if p == "" {
		writeError(w, http.StatusBadRequest, "path is required: the SQL file to load, from the server's files")
		return
	}
	if outsideRoot(w, p) {
		return
	}
	rel := path.Clean("/" + p)
	if rel == "/" {
		writeError(w, http.StatusBadRequest, "path names the data directory, not a file")
		return
	}
	wipe := true
	if req.Wipe != nil {
		wipe = *req.Wipe
	}
	b := &models.Backup{
		ServerID: srv.ID, Direction: models.DirDatabaseImport, Phase: models.BackupPending,
		DatabaseID: d.ID, Path: rel, Wipe: wipe,
	}
	switch err := s.Store.CreateDatabaseImport(b); {
	case errors.Is(err, store.ErrServerRunning):
		writeError(w, http.StatusConflict, "stop the server before importing: the game would write to the database while the file replaces it")
		return
	case errors.Is(err, store.ErrDatabaseImportActive):
		writeError(w, http.StatusConflict, "an import is already waiting or running for this server; wait for it to finish, or cancel it")
		return
	case errors.Is(err, store.ErrRestoreActive):
		writeError(w, http.StatusConflict, restoreActiveMessage)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	detail := fmt.Sprintf("%s into %s", rel, d.DatabaseName)
	if wipe {
		detail += ", emptied first"
	}
	s.audit(r, srv.ID, "database.import", detail)
	writeJSON(w, http.StatusAccepted, b)
}

// handleCancelDatabaseImport calls off an import that has not started.
func (s *Server) handleCancelDatabaseImport(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermDatabases)
	if !ok {
		return
	}
	d, ok := s.lookupServerDatabase(w, r, srv.ID)
	if !ok {
		return
	}
	latest, err := s.Store.LatestDatabaseImports(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	b, ok := latest[d.ID]
	switch {
	case !ok || (b.Phase != models.BackupPending && b.Phase != models.BackupRunning):
		writeError(w, http.StatusNotFound, "no import is waiting for this database")
		return
	case b.Phase == models.BackupRunning:
		writeError(w, http.StatusConflict, "the import is loading the file; it cannot be stopped half way")
		return
	}
	cancelled, err := s.Store.CancelPendingBackup(b.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !cancelled {
		writeError(w, http.StatusConflict, "the import has just started; it cannot be stopped half way")
		return
	}
	s.audit(r, srv.ID, "database.import-cancel", d.DatabaseName)
	w.WriteHeader(http.StatusNoContent)
}

// databaseImportInProgress refuses, with a 409, what an import waiting or
// running forbids: moving the server elsewhere under it.
func (s *Server) databaseImportInProgress(w http.ResponseWriter, srv *models.Server) bool {
	active, err := s.Store.HasActiveDatabaseImport(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return true
	}
	if active {
		writeError(w, http.StatusConflict, databaseImportActiveMessage)
		return true
	}
	return false
}

// importView is the last import of a database, as the databases list shows it.
func importView(b models.Backup) map[string]any {
	v := map[string]any{
		"id": b.ID, "phase": b.Phase, "path": b.Path, "wipe": b.Wipe, "createdAt": b.CreatedAt,
	}
	if b.Message != "" {
		v["message"] = b.Message
	}
	if b.CompletedAt != nil {
		v["completedAt"] = b.CompletedAt
	}
	return v
}
