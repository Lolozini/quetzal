package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/dbprovision"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// ---- database hosts (admin registry) ----

func (s *Server) handleListDatabaseHosts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermDatabaseHosts) {
		return
	}
	hs, err := s.Store.ListDatabaseHosts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Annotate each host with how many databases it holds (for the admin UI).
	type hostView struct {
		models.DatabaseHost
		Databases int64 `json:"databases"`
	}
	out := make([]hostView, 0, len(hs))
	for i := range hs {
		n, _ := s.Store.CountDatabasesOnHost(hs[i].ID)
		out = append(out, hostView{DatabaseHost: hs[i], Databases: n})
	}
	writeJSON(w, http.StatusOK, out)
}

type databaseHostRequest struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind"` // external | managed
	Host          string  `json:"host"`
	Port          int     `json:"port"`
	ConnectHost   string  `json:"connectHost"`
	ConnectPort   int     `json:"connectPort"`
	AdminUser     string  `json:"adminUser"`
	AdminPassword *string `json:"adminPassword"` // nil keeps existing on update
	MaxDatabases  int     `json:"maxDatabases"`
	// Managed-only.
	ClusterID   uint   `json:"clusterId"`
	Namespace   string `json:"namespace"`
	Image       string `json:"image"`
	StorageSize string `json:"storageSize"`
}

func (s *Server) handleCreateDatabaseHost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermDatabaseHosts) {
		return
	}
	var req databaseHostRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = models.DBHostExternal
	}
	if kind != models.DBHostExternal && kind != models.DBHostManaged {
		writeError(w, http.StatusBadRequest, "kind must be 'external' or 'managed'")
		return
	}

	h := &models.DatabaseHost{
		Name: req.Name, Kind: kind,
		ConnectHost: strings.TrimSpace(req.ConnectHost), ConnectPort: req.ConnectPort,
		MaxDatabases: req.MaxDatabases,
	}
	adminPassword := ""
	if req.AdminPassword != nil {
		adminPassword = *req.AdminPassword
	}

	if kind == models.DBHostExternal {
		h.Host = strings.TrimSpace(req.Host)
		h.Port = req.Port
		h.AdminUser = strings.TrimSpace(req.AdminUser)
		if h.Host == "" || h.Port == 0 || h.AdminUser == "" || adminPassword == "" {
			writeError(w, http.StatusBadRequest, "host, port, adminUser and adminPassword are required for an external host")
			return
		}
	} else {
		// Managed: Quetzal owns the workload and root credentials; the admin only
		// chooses where/how big. The controller reconciles the MariaDB; the admin
		// password is a generated root password. Host/Namespace derive from the ID
		// (assigned on create), so they're filled in just below.
		h.ClusterID = req.ClusterID
		h.Namespace = strings.TrimSpace(req.Namespace)
		h.Image = strings.TrimSpace(req.Image)
		if h.Image == "" {
			h.Image = reconciler.DefaultMariaDBImage
		}
		h.StorageSize = strings.TrimSpace(req.StorageSize)
		if h.StorageSize == "" {
			h.StorageSize = "1Gi"
		}
		h.AdminUser = "root"
		h.Port = 3306
		adminPassword = dbprovision.GeneratePassword()
	}

	if err := s.Store.CreateDatabaseHost(h, adminPassword); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Managed hosts: the namespace/Service DNS depend on the assigned ID.
	if kind == models.DBHostManaged {
		if h.Namespace == "" {
			h.Namespace = reconciler.ManagedDBNamespace(h)
		}
		h.Host = reconciler.ManagedDBServiceHost(h)
		_ = s.Store.UpdateDatabaseHost(h, nil)
	}
	s.audit(r, 0, "dbhost.create", h.Name)
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) handleUpdateDatabaseHost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermDatabaseHosts) {
		return
	}
	h, ok := s.lookupDatabaseHost(w, r)
	if !ok {
		return
	}
	var req databaseHostRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if n := strings.TrimSpace(req.Name); n != "" {
		h.Name = n
	}
	h.ConnectHost = strings.TrimSpace(req.ConnectHost)
	h.ConnectPort = req.ConnectPort
	h.MaxDatabases = req.MaxDatabases
	if h.Kind == models.DBHostExternal {
		if hh := strings.TrimSpace(req.Host); hh != "" {
			h.Host = hh
		}
		if req.Port != 0 {
			h.Port = req.Port
		}
		if au := strings.TrimSpace(req.AdminUser); au != "" {
			h.AdminUser = au
		}
	} else {
		h.StorageSize = strings.TrimSpace(req.StorageSize)
		if img := strings.TrimSpace(req.Image); img != "" {
			h.Image = img
		}
	}
	// Only external hosts accept an admin-password change (managed roots are
	// Quetzal-owned). A nil password keeps the stored one.
	var pw *string
	if h.Kind == models.DBHostExternal {
		pw = req.AdminPassword
	}
	if err := s.Store.UpdateDatabaseHost(h, pw); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "dbhost.update", h.Name)
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleDeleteDatabaseHost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermDatabaseHosts) {
		return
	}
	h, ok := s.lookupDatabaseHost(w, r)
	if !ok {
		return
	}
	if n, _ := s.Store.CountDatabasesOnHost(h.ID); n > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("host still has %d database(s); delete them first", n))
		return
	}
	if err := s.Store.DeleteDatabaseHost(h.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The controller GCs the managed workload once the row is gone.
	s.audit(r, 0, "dbhost.delete", h.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestDatabaseHost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermDatabaseHosts) {
		return
	}
	h, ok := s.lookupDatabaseHost(w, r)
	if !ok {
		return
	}
	conn, err := s.adminConn(h)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	pingErr := dbprovision.Ping(ctx, conn)
	msg := ""
	if pingErr != nil {
		msg = pingErr.Error()
	}
	_ = s.Store.SetDatabaseHostStatus(h.ID, pingErr == nil, msg)
	updated, _ := s.Store.GetDatabaseHost(h.ID)
	writeJSON(w, http.StatusOK, updated)
}

// ---- per-server databases ----

func (s *Server) handleListServerDatabases(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermDatabases)
	if !ok {
		return
	}
	ds, err := s.Store.ListServerDatabases(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(ds))
	for i := range ds {
		out = append(out, s.databaseView(&ds[i], false))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListServerDatabaseHosts returns the hosts a server may create databases
// on (minimal view, no admin secrets), for users with the databases permission
// who aren't admins and so can't read the full host registry.
func (s *Server) handleListServerDatabaseHosts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireServer(w, r, models.PermDatabases); !ok {
		return
	}
	hs, err := s.Store.ListDatabaseHosts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(hs))
	for i := range hs {
		n, _ := s.Store.CountDatabasesOnHost(hs[i].ID)
		out = append(out, map[string]any{
			"id":   hs[i].ID,
			"name": hs[i].Name,
			"kind": hs[i].Kind,
			"full": hs[i].MaxDatabases > 0 && n >= int64(hs[i].MaxDatabases),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateServerDatabase(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermDatabases)
	if !ok {
		return
	}
	var req struct {
		HostID uint `json:"hostId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	host, err := s.Store.GetDatabaseHost(req.HostID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown database host")
		return
	}
	if host.MaxDatabases > 0 {
		if n, _ := s.Store.CountDatabasesOnHost(host.ID); n >= int64(host.MaxDatabases) {
			writeError(w, http.StatusConflict, "database host is at capacity")
			return
		}
	}
	conn, err := s.adminConn(host)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d := &models.ServerDatabase{
		ServerID:     srv.ID,
		HostID:       host.ID,
		DatabaseName: dbprovision.GenerateName("s", srv.ID),
		Username:     dbprovision.GenerateName("u", srv.ID),
		Remote:       "%",
	}
	password := dbprovision.GeneratePassword()

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := dbprovision.Provision(ctx, conn, d.DatabaseName, d.Username, d.Remote, password); err != nil {
		writeError(w, http.StatusBadGateway, "could not provision database: "+err.Error())
		return
	}
	if err := s.Store.CreateServerDatabase(d, password); err != nil {
		// Best-effort rollback so we don't leak an unmanaged database.
		_ = dbprovision.Deprovision(ctx, conn, d.DatabaseName, d.Username, d.Remote)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "database.create", d.DatabaseName)
	resp := s.databaseView(d, true)
	resp["password"] = password
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleGetServerDatabase(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermDatabases)
	if !ok {
		return
	}
	d, ok := s.lookupServerDatabase(w, r, srv.ID)
	if !ok {
		return
	}
	pw, err := s.Store.ServerDatabasePassword(d)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read credentials")
		return
	}
	resp := s.databaseView(d, true)
	resp["password"] = pw
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRotateServerDatabase(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermDatabases)
	if !ok {
		return
	}
	d, ok := s.lookupServerDatabase(w, r, srv.ID)
	if !ok {
		return
	}
	host, err := s.Store.GetDatabaseHost(d.HostID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "host unavailable")
		return
	}
	conn, err := s.adminConn(host)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	password := dbprovision.GeneratePassword()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := dbprovision.RotatePassword(ctx, conn, d.Username, d.Remote, password); err != nil {
		writeError(w, http.StatusBadGateway, "could not rotate password: "+err.Error())
		return
	}
	if err := s.Store.UpdateServerDatabasePassword(d.ID, password); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "database.rotate", d.DatabaseName)
	resp := s.databaseView(d, true)
	resp["password"] = password
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteServerDatabase(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermDatabases)
	if !ok {
		return
	}
	d, ok := s.lookupServerDatabase(w, r, srv.ID)
	if !ok {
		return
	}
	if host, err := s.Store.GetDatabaseHost(d.HostID); err == nil {
		if conn, err := s.adminConn(host); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			// Best-effort drop; the row is removed regardless so the panel stays
			// consistent even if the host is temporarily unreachable.
			_ = dbprovision.Deprovision(ctx, conn, d.DatabaseName, d.Username, d.Remote)
		}
	}
	if err := s.Store.DeleteServerDatabase(d.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "database.delete", d.DatabaseName)
	w.WriteHeader(http.StatusNoContent)
}

// dropServerDatabases deprovisions and removes every database of a server (used
// when the server is deleted). Best-effort on the host side: the row is removed
// regardless, so a deleted server never leaves dangling panel state.
func (s *Server) dropServerDatabases(ctx context.Context, serverID uint) {
	ds, err := s.Store.ListServerDatabases(serverID)
	if err != nil {
		return
	}
	for i := range ds {
		d := &ds[i]
		if host, err := s.Store.GetDatabaseHost(d.HostID); err == nil {
			if conn, err := s.adminConn(host); err == nil {
				dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				_ = dbprovision.Deprovision(dctx, conn, d.DatabaseName, d.Username, d.Remote)
				cancel()
			}
		}
		_ = s.Store.DeleteServerDatabase(d.ID)
	}
}

// ---- helpers ----

// adminConn builds an admin connection to a host, decrypting its password.
func (s *Server) adminConn(h *models.DatabaseHost) (dbprovision.Conn, error) {
	pw, err := s.Store.DatabaseHostAdminPassword(h)
	if err != nil {
		return dbprovision.Conn{}, err
	}
	host, port := h.AdminAddr()
	return dbprovision.Conn{Host: host, Port: port, User: h.AdminUser, Password: pw}, nil
}

// databaseView renders a database for the API. Connection details point at the
// host's client address; the password is added by the caller when allowed.
func (s *Server) databaseView(d *models.ServerDatabase, withConn bool) map[string]any {
	v := map[string]any{
		"id":           d.ID,
		"serverId":     d.ServerID,
		"hostId":       d.HostID,
		"databaseName": d.DatabaseName,
		"username":     d.Username,
		"remote":       d.Remote,
		"createdAt":    d.CreatedAt,
	}
	if host, err := s.Store.GetDatabaseHost(d.HostID); err == nil {
		v["host"] = host.ClientHost()
		v["port"] = host.ClientPort()
		v["hostName"] = host.Name
	}
	_ = withConn
	return v
}

func (s *Server) lookupDatabaseHost(w http.ResponseWriter, r *http.Request) (*models.DatabaseHost, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("hid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid host id")
		return nil, false
	}
	h, err := s.Store.GetDatabaseHost(uint(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "host not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return h, true
}

// lookupServerDatabase loads a database by path id and confirms it belongs to
// the given server (so one server can't touch another's databases).
func (s *Server) lookupServerDatabase(w http.ResponseWriter, r *http.Request, serverID uint) (*models.ServerDatabase, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("dbid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return nil, false
	}
	d, err := s.Store.GetServerDatabase(uint(id))
	if err != nil || d.ServerID != serverID {
		writeError(w, http.StatusNotFound, "database not found")
		return nil, false
	}
	return d, true
}
