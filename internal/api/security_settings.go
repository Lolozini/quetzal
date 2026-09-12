package api

import (
	"net/http"
	"strings"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// handleGetSecuritySettings returns the panel-wide authentication policy.
// Readable by any admin (knowing whether 2FA is required is not sensitive);
// changing it is superadmin-only, like the email settings, because it decides
// who can get in.
func (s *Server) handleGetSecuritySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermSettings) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requireTwoFactor": s.requireTwoFactorPolicy(),
		"options":          []string{store.Require2FAOff, store.Require2FAAdmins, store.Require2FAAll},
	})
}

type securitySettingsRequest struct {
	RequireTwoFactor string `json:"requireTwoFactor"`
}

// handleSetSecuritySettings sets who must hold a second factor. Superadmin only.
func (s *Server) handleSetSecuritySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req securitySettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	v := strings.TrimSpace(req.RequireTwoFactor)
	switch v {
	case "", store.Require2FAOff:
		v = store.Require2FAOff
	case store.Require2FAAdmins, store.Require2FAAll:
	default:
		writeError(w, http.StatusBadRequest, `requireTwoFactor must be "off", "admins" or "all"`)
		return
	}
	// Turning the requirement on locks nobody out: a user without a second
	// factor keeps a session, but it only reaches the enrolment endpoints until
	// they have one (see twoFactorGate). Refusing the login instead would strand
	// every account at once, the superadmin included.
	if err := s.Store.SetSetting(store.SettingRequire2FA, v); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "settings.require-2fa", v)
	writeJSON(w, http.StatusOK, map[string]any{"requireTwoFactor": v})
}

// requireTwoFactorPolicy reads the policy, defaulting to off.
func (s *Server) requireTwoFactorPolicy() string {
	v, _ := s.Store.GetSetting(store.SettingRequire2FA)
	switch strings.TrimSpace(v) {
	case store.Require2FAAdmins:
		return store.Require2FAAdmins
	case store.Require2FAAll:
		return store.Require2FAAll
	}
	return store.Require2FAOff
}

// twoFactorMissing reports whether the policy applies to this user and they have
// not enrolled yet.
func (s *Server) twoFactorMissing(u *models.User) bool {
	if u == nil || u.TOTPEnabled {
		return false
	}
	switch s.requireTwoFactorPolicy() {
	case store.Require2FAAll:
		return true
	case store.Require2FAAdmins:
		return u.IsAnyAdmin()
	}
	return false
}

// twoFactorExempt lists what a user still reaches while they owe a second
// factor: reading who they are, enrolling, and logging out. Anything else would
// let the requirement be ignored; anything less would make it impossible to
// satisfy.
func twoFactorExempt(path string) bool {
	switch path {
	case "/api/me", "/api/logout", "/api/me/2fa/setup", "/api/me/2fa/enable", "/api/version", "/api/healthz":
		return true
	}
	return false
}
