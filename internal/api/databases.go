package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/dbprovision"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// Kept as functions so tests can control remote completion without a live host.
var provisionDatabase = dbprovision.Provision
var deprovisionDatabase = dbprovision.Deprovision
var rotateDatabasePassword = dbprovision.RotatePassword

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
		// The namespace is Quetzal's to choose, not the caller's. It creates this
		// namespace and deletes it when the host goes, so a name pointing at one
		// that already exists would place a workload in someone else's namespace
		// and then collect it — kube-system included. An explicit name is accepted
		// only when it is one Quetzal could have picked itself.
		if n := strings.TrimSpace(req.Namespace); n != "" {
			if !models.IsManagedDBNamespace(n) {
				writeError(w, http.StatusBadRequest,
					`namespace must be named "quetzal-db-<name>" — Quetzal owns and deletes the namespace of a managed host, so it will not take over one it did not create`)
				return
			}
			h.Namespace = n
		}
		h.Image = strings.TrimSpace(req.Image)
		if h.Image == "" {
			h.Image = reconciler.DefaultMariaDBImage
		}
		h.StorageSize = strings.TrimSpace(req.StorageSize)
		if h.StorageSize == "" {
			h.StorageSize = "1Gi"
		}
		// Lands in the same resource.MustParse as a server's volume, in the same
		// reconcile loop; see validateStorageSize.
		if err := validateStorageSize(h.StorageSize); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.AdminUser = "root"
		h.Port = 3306
		adminPassword = dbprovision.GeneratePassword()
	}

	if err := s.Store.CreateDatabaseHost(h, adminPassword); err != nil {
		if errors.Is(err, store.ErrDatabaseHostNamespaceInUse) {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// Managed hosts: the namespace/Service DNS depend on the assigned ID.
	if kind == models.DBHostManaged {
		if h.Namespace == "" {
			h.Namespace = h.ManagedNamespace()
		}
		h.Host = reconciler.ManagedDBServiceHost(h)
		if err := s.Store.UpdateDatabaseHost(h, nil); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.audit(r, 0, "dbhost.create", h.Name)
	writeJSON(w, http.StatusCreated, s.checkedHost(r, h))
}

// checkedHost checks an external host at once and returns it as stored, status
// included. A managed one is not up yet when it is created: the controller
// checks it on each pass until it answers.
func (s *Server) checkedHost(r *http.Request, h *models.DatabaseHost) *models.DatabaseHost {
	if h.Kind != models.DBHostExternal || s.CheckDatabaseHost == nil {
		return h
	}
	s.CheckDatabaseHost(r.Context(), h)
	if got, err := s.Store.GetDatabaseHost(h.ID); err == nil {
		return got
	}
	return h
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
		if size := strings.TrimSpace(req.StorageSize); size != "" {
			if err := validateStorageSize(size); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			h.StorageSize = size
		}
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
		if errors.Is(err, store.ErrDatabaseHostNamespaceInUse) {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.audit(r, 0, "dbhost.update", h.Name)
	writeJSON(w, http.StatusOK, s.checkedHost(r, h))
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
	imports, err := s.Store.LatestDatabaseImports(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(ds))
	for i := range ds {
		v := s.databaseView(&ds[i], false)
		if b, ok := imports[ds[i].ID]; ok {
			v["lastImport"] = importView(b)
		}
		out = append(out, v)
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
	conn, err := s.adminConn(host)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Remote is MySQL's "may connect from" and stays "%" deliberately. Narrowing
	// it would have to separate one tenant from another by source address, and
	// there is nothing to separate them by: every pod draws from one cluster-wide
	// CIDR and its address changes on each restart. A range covering the pod
	// network would admit exactly the same set while being one more thing to get
	// wrong. What keeps tenants apart is the grant — each user is granted only
	// its own database (see internal/dbprovision) — and, for a managed host, the
	// ingress NetworkPolicy that admits only the namespaces holding a database
	// there (see reconciler.BuildManagedDBNetworkPolicy).
	d := &models.ServerDatabase{
		ServerID:     srv.ID,
		HostID:       host.ID,
		DatabaseName: dbprovision.GenerateName("s", srv.ID),
		Username:     dbprovision.GenerateName("u", srv.ID),
		Remote:       "%",
	}
	password := dbprovision.GeneratePassword()
	// Commit the reservation first, so every replica counts it while the
	// remote operation is in flight (and after an uncertain remote failure).
	if err := s.Store.CreateServerDatabase(d, password); err != nil {
		if errors.Is(err, store.ErrDatabaseHostFull) {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := provisionDatabase(ctx, conn, d.DatabaseName, d.Username, d.Remote, password); err != nil {
		// A failed response does not prove that CREATE failed. Only release the
		// reservation after confirmed cleanup, using a fresh bounded context
		// even when the requesting client has disconnected.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
		defer cleanupCancel()
		cleanupErr := deprovisionDatabase(cleanupCtx, conn, d.DatabaseName, d.Username, d.Remote)
		if cleanupErr != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("could not provision database: %v; cleanup failed: %v; database reservation %d retained", err, cleanupErr, d.ID))
			return
		}
		if cleanupErr := s.Store.DeleteServerDatabase(d.ID); cleanupErr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("could not release database reservation %d: %v", d.ID, cleanupErr))
			return
		}
		writeError(w, http.StatusBadGateway, "could not provision database: "+err.Error())
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
	err = s.Store.RotateServerDatabasePassword(d.ID, password, func(current *models.ServerDatabase, next string) error {
		ctx := r.Context()
		if next != password {
			ctx = context.WithoutCancel(ctx) // compensation must survive disconnects
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return rotateDatabasePassword(ctx, conn, current.Username, current.Remote, next)
	})
	if err != nil {
		if errors.Is(err, store.ErrDatabaseRotationRemote) {
			writeError(w, http.StatusBadGateway, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
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
	if active, err := s.Store.DatabaseImportActive(d.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if active {
		writeError(w, http.StatusConflict, "an import into this database is waiting or running; cancel it, or wait for it to finish")
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
	if err := s.Store.DeleteDatabaseImports(d.ID); err != nil {
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
	id, ok := pathID(r, "hid")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid host id")
		return nil, false
	}
	h, err := s.Store.GetDatabaseHost(id)
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
	id, ok := pathID(r, "dbid")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return nil, false
	}
	d, err := s.Store.GetServerDatabase(id)
	if err != nil || d.ServerID != serverID {
		writeError(w, http.StatusNotFound, "database not found")
		return nil, false
	}
	return d, true
}
