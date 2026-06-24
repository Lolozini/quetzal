package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// Admin roles bundle scoped admin permissions for delegation. Managing them and
// assigning them to users is strictly superadmin territory (requireAdmin): it
// governs the admin-privilege system itself, so a scoped admin must not be able
// to widen their own access.

// adminPermInfo describes a single admin permission for the UI catalog.
type adminPermInfo struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// adminPermCatalog is the authoritative, human-readable list of admin
// permissions, kept in declaration order.
var adminPermCatalog = []adminPermInfo{
	{models.AdminPermServers, "Administer every server (view, power, files, settings, delete, suspend)"},
	{models.AdminPermUsers, "Manage user accounts (not admin status, which stays superadmin-only)"},
	{models.AdminPermTemplates, "Import, edit and delete templates (eggs)"},
	{models.AdminPermClusters, "Manage the cluster registry"},
	{models.AdminPermDatabaseHosts, "Manage database hosts"},
	{models.AdminPermNotifications, "Manage global notification channels"},
	{models.AdminPermSettings, "Configure email/SMTP and backups"},
	{models.AdminPermAudit, "View the global activity log"},
}

// handleAdminPermissionCatalog returns the catalog of admin permissions so the
// UI can render role editors authoritatively. Available to superadmins.
func (s *Server) handleAdminPermissionCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, adminPermCatalog)
}

func (s *Server) handleListAdminRoles(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rs, err := s.Store.ListAdminRoles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

type adminRoleRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

// sanitizeAdminPerms validates and de-duplicates a permission list, preserving
// the canonical catalog order.
func sanitizeAdminPerms(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	for _, p := range in {
		if !models.ValidAdminPermission(p) {
			return nil, errors.New("invalid permission: " + p)
		}
		seen[p] = true
	}
	out := make([]string, 0, len(seen))
	for _, p := range models.AllAdminPermissions {
		if seen[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Server) handleCreateAdminRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req adminRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 190 {
		writeError(w, http.StatusBadRequest, "name is required (<=190 chars)")
		return
	}
	perms, err := sanitizeAdminPerms(req.Permissions)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	role := &models.AdminRole{Name: name, Description: strings.TrimSpace(req.Description), Permissions: perms}
	if err := s.Store.CreateAdminRole(role); err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a role with that name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "adminrole.create", role.Name)
	writeJSON(w, http.StatusCreated, role)
}

func (s *Server) handleUpdateAdminRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	role, ok := s.lookupAdminRole(w, r)
	if !ok {
		return
	}
	var req adminRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 190 {
		writeError(w, http.StatusBadRequest, "name is required (<=190 chars)")
		return
	}
	perms, err := sanitizeAdminPerms(req.Permissions)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.UpdateAdminRole(role.ID, name, strings.TrimSpace(req.Description), perms); err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a role with that name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "adminrole.update", name)
	updated, _ := s.Store.GetAdminRole(role.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteAdminRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	role, ok := s.lookupAdminRole(w, r)
	if !ok {
		return
	}
	n, err := s.Store.CountUsersByAdminRole(role.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n > 0 {
		writeError(w, http.StatusConflict, "role is assigned to "+strconv.FormatInt(n, 10)+" user(s); reassign them first")
		return
	}
	if err := s.Store.DeleteAdminRole(role.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "adminrole.delete", role.Name)
	w.WriteHeader(http.StatusNoContent)
}

// handleSetUserAdminRole assigns or clears a user's scoped admin role.
func (s *Server) handleSetUserAdminRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	target, ok := s.lookupUser(w, r)
	if !ok {
		return
	}
	var req struct {
		RoleID *uint `json:"roleId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if target.IsAdmin {
		writeError(w, http.StatusConflict, "superadmins already hold every admin permission")
		return
	}
	detail := target.Username + ": none"
	if req.RoleID != nil {
		role, err := s.Store.GetAdminRole(*req.RoleID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "no such role")
			return
		}
		detail = target.Username + ": " + role.Name
	}
	if err := s.Store.SetUserAdminRole(target.ID, req.RoleID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "user.adminrole", detail)
	updated, _ := s.Store.GetUser(target.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) lookupAdminRole(w http.ResponseWriter, r *http.Request) (*models.AdminRole, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("rid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid role id")
		return nil, false
	}
	role, err := s.Store.GetAdminRole(uint(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "role not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return role, true
}

// isUniqueViolation reports whether err is a unique-constraint violation, across
// SQLite and Postgres (matched by message since drivers differ).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}
