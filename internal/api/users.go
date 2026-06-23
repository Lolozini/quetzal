package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// ---- user management (admin) ----

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	us, err := s.Store.ListUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, us)
}

type createUserRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	Email       string `json:"email"`
	IsAdmin     bool   `json:"isAdmin"`
	MaxServers  int    `json:"maxServers"`
	MaxMemoryMB int64  `json:"maxMemoryMB"`
	MaxCPUMilli int64  `json:"maxCpuMilli"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(req.Username) < 3 || len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "username >=3 and password >=8 chars required")
		return
	}
	if _, err := s.Store.GetUserByUsername(req.Username); err == nil {
		writeError(w, http.StatusConflict, "username already taken")
		return
	}
	email := strings.TrimSpace(req.Email)
	if email != "" && !looksLikeEmail(email) {
		writeError(w, http.StatusBadRequest, "invalid email")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash failed")
		return
	}
	u := &models.User{
		Username: req.Username, PasswordHash: hash, Email: email, IsAdmin: req.IsAdmin,
		MaxServers: req.MaxServers, MaxMemoryMB: req.MaxMemoryMB, MaxCPUMilli: req.MaxCPUMilli,
	}
	if err := s.Store.CreateUser(u); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "user.create", u.Username)
	writeJSON(w, http.StatusCreated, u)
}

type updateUserRequest struct {
	IsAdmin     bool    `json:"isAdmin"`
	MaxServers  int     `json:"maxServers"`
	MaxMemoryMB int64   `json:"maxMemoryMB"`
	MaxCPUMilli int64   `json:"maxCpuMilli"`
	Password    *string `json:"password,omitempty"` // optional reset
	Email       *string `json:"email,omitempty"`    // optional set/clear
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	target, ok := s.lookupUser(w, r)
	if !ok {
		return
	}
	var req updateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	// Don't allow demoting the last admin.
	if target.IsAdmin && !req.IsAdmin {
		if n, _ := s.Store.CountAdmins(); n <= 1 {
			writeError(w, http.StatusConflict, "cannot demote the last admin")
			return
		}
	}
	if err := s.Store.UpdateUserAdminFields(target.ID, req.IsAdmin, req.MaxServers, req.MaxMemoryMB, req.MaxCPUMilli); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Password != nil {
		if len(*req.Password) < 8 {
			writeError(w, http.StatusBadRequest, "password must be >=8 chars")
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "hash failed")
			return
		}
		if err := s.Store.UpdateUserPassword(target.ID, hash); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		if email != "" && !looksLikeEmail(email) {
			writeError(w, http.StatusBadRequest, "invalid email")
			return
		}
		if err := s.Store.UpdateUserEmail(target.ID, email); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.audit(r, 0, "user.update", target.Username)
	updated, _ := s.Store.GetUser(target.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	target, ok := s.lookupUser(w, r)
	if !ok {
		return
	}
	if me := userFrom(r.Context()); me != nil && me.ID == target.ID {
		writeError(w, http.StatusConflict, "cannot delete your own account")
		return
	}
	if target.IsAdmin {
		if n, _ := s.Store.CountAdmins(); n <= 1 {
			writeError(w, http.StatusConflict, "cannot delete the last admin")
			return
		}
	}
	if err := s.Store.DeleteUser(target.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "user.delete", target.Username)
	w.WriteHeader(http.StatusNoContent)
}

// handleChangePassword lets the current user change their own password.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if ok, _ := auth.VerifyPassword(u.PasswordHash, req.OldPassword); !ok {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "new password must be >=8 chars")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash failed")
		return
	}
	if err := s.Store.UpdateUserPassword(u.ID, hash); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetMyEmail lets the current user set or clear their own email (used for
// self-service password reset). Emails are not verified.
func (s *Server) handleSetMyEmail(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var req struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	email := strings.TrimSpace(req.Email)
	if email != "" && !looksLikeEmail(email) {
		writeError(w, http.StatusBadRequest, "invalid email")
		return
	}
	if err := s.Store.UpdateUserEmail(u.ID, email); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, _ := s.Store.GetUser(u.ID)
	writeJSON(w, http.StatusOK, updated)
}

// looksLikeEmail is a light sanity check (not full RFC validation): one '@' with
// non-empty local and domain parts and no whitespace.
func looksLikeEmail(s string) bool {
	s = strings.TrimSpace(s)
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t\r\n")
}

func (s *Server) lookupUser(w http.ResponseWriter, r *http.Request) (*models.User, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("uid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return nil, false
	}
	u, err := s.Store.GetUser(uint(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return u, true
}

// ---- subusers (per-server access grants) ----

// requireOwnerOrAdmin loads the server and ensures the caller owns it (or is
// admin). Managing subusers is reserved to owners/admins, not other subusers.
func (s *Server) requireOwnerOrAdmin(w http.ResponseWriter, r *http.Request) (*models.Server, bool) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return nil, false
	}
	u := userFrom(r.Context())
	if u != nil && (u.IsAdmin || srv.OwnerID == u.ID) {
		return srv, true
	}
	// Hide existence from users who can't even view it.
	if s.can(u, srv, models.PermView) {
		writeError(w, http.StatusForbidden, "only the owner can manage access")
	} else {
		writeError(w, http.StatusNotFound, "server not found")
	}
	return nil, false
}

func (s *Server) handleListAccess(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireOwnerOrAdmin(w, r)
	if !ok {
		return
	}
	as, err := s.Store.ListAccessForServer(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, as)
}

type grantAccessRequest struct {
	Username    string   `json:"username"`
	Permissions []string `json:"permissions"`
}

func (s *Server) handleGrantAccess(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireOwnerOrAdmin(w, r)
	if !ok {
		return
	}
	var req grantAccessRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	target, err := s.Store.GetUserByUsername(strings.TrimSpace(req.Username))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such user")
		return
	}
	if target.ID == srv.OwnerID {
		writeError(w, http.StatusBadRequest, "user already owns this server")
		return
	}
	if len(req.Permissions) == 0 {
		writeError(w, http.StatusBadRequest, "at least one permission is required")
		return
	}
	for _, p := range req.Permissions {
		if !models.ValidPermission(p) {
			writeError(w, http.StatusBadRequest, "invalid permission: "+p)
			return
		}
	}
	if err := s.Store.GrantAccess(srv.ID, target.ID, req.Permissions); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "access.grant", target.Username+": "+strings.Join(req.Permissions, ","))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeAccess(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireOwnerOrAdmin(w, r)
	if !ok {
		return
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("uid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if err := s.Store.RevokeAccess(srv.ID, uint(uid)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "access.revoke", strconv.FormatUint(uid, 10))
	w.WriteHeader(http.StatusNoContent)
}

// ---- audit ----

func (s *Server) handleServerAudit(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermView)
	if !ok {
		return
	}
	es, err := s.Store.ListAuditForServer(srv.ID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, es)
}

func (s *Server) handleGlobalAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	es, err := s.Store.ListAudit(200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, es)
}
