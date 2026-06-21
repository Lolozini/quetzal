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
	if u.IsAdmin || srv.OwnerID == u.ID {
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

// requireAdmin gates an action to admins.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u := userFrom(r.Context())
	if u == nil || !u.IsAdmin {
		writeError(w, http.StatusForbidden, "admin privileges required")
		return false
	}
	return true
}

// audit records a mutating action (best-effort; failures never block).
func (s *Server) audit(r *http.Request, serverID uint, action, detail string) {
	u := userFrom(r.Context())
	e := &models.AuditEntry{ServerID: serverID, Action: action, Detail: detail}
	if u != nil {
		e.UserID = u.ID
		e.Username = u.Username
	}
	_ = s.Store.AddAudit(e)
}
