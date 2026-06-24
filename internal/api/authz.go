package api

import (
	"net/http"

	"github.com/lolozini/quetzal/internal/models"
)

// can reports whether a user holds a permission on a server. Admins and the
// owner hold all permissions; subusers hold their granted subset.
func (s *Server) can(u *models.User, srv *models.Server, perm string) bool {
	if u == nil {
		return false
	}
	if u.HasAdminPerm(models.AdminPermServers) || srv.OwnerID == u.ID {
		return true
	}
	acc, err := s.Store.GetServerAccess(srv.ID, u.ID)
	if err != nil {
		return false
	}
	return acc.Has(perm)
}

// requireServer loads the server in the path and checks the current user holds
// `perm` on it. To avoid leaking existence, a user with no access at all gets
// 404; one who can view but lacks the specific permission gets 403.
func (s *Server) requireServer(w http.ResponseWriter, r *http.Request, perm string) (*models.Server, bool) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return nil, false
	}
	u := userFrom(r.Context())
	if s.can(u, srv, perm) {
		return srv, true
	}
	if s.can(u, srv, models.PermView) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
	} else {
		writeError(w, http.StatusNotFound, "server not found")
	}
	return nil, false
}

// requireAdmin gates an action to superadmins (User.IsAdmin). Reserved for
// managing the admin-privilege system itself (admin roles, granting/revoking
// admin status). Capability-scoped actions use requireAdminPerm instead.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u := userFrom(r.Context())
	if u == nil || !u.IsAdmin {
		writeError(w, http.StatusForbidden, "admin privileges required")
		return false
	}
	return true
}

// requireAdminPerm gates an action to users holding a specific admin permission.
// Superadmins hold all of them; scoped admins hold the subset from their role.
func (s *Server) requireAdminPerm(w http.ResponseWriter, r *http.Request, perm string) bool {
	if u := userFrom(r.Context()); u.HasAdminPerm(perm) {
		return true
	}
	writeError(w, http.StatusForbidden, "admin privileges required")
	return false
}

// audit records a mutating action (best-effort; failures never block) and emits
// a matching event so the action can fan out to notification channels.
func (s *Server) audit(r *http.Request, serverID uint, action, detail string) {
	u := userFrom(r.Context())
	e := &models.AuditEntry{ServerID: serverID, Action: action, Detail: detail}
	if u != nil {
		e.UserID = u.ID
		e.Username = u.Username
	}
	_ = s.Store.AddAudit(e)
	s.emit(r, serverID, action, detail, nil)
}

// emit appends an event and, when notifications are enabled, nudges the
// dispatcher for prompt delivery. Best-effort: failures never block the action.
// For server-scoped events the server slug is prepended to the message so
// notifications read naturally without a lookup downstream.
func (s *Server) emit(r *http.Request, serverID uint, eventType, message string, data map[string]string) {
	e := &models.Event{ServerID: serverID, Type: eventType, Message: message, Data: data}
	if u := userFrom(r.Context()); u != nil {
		e.UserID = u.ID
		e.Username = u.Username
	}
	if serverID != 0 {
		if srv, err := s.Store.GetServer(serverID); err == nil {
			if message != "" {
				e.Message = srv.Slug + ": " + message
			} else {
				e.Message = srv.Slug
			}
		}
	}
	if err := s.Store.AddEvent(e); err != nil {
		return
	}
	if s.Dispatch != nil {
		s.Dispatch.Notify()
	}
}
