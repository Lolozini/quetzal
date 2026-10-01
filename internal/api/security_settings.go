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
	impact, err := s.twoFactorImpact()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requireTwoFactor": s.requireTwoFactorPolicy(),
		"options":          []string{store.Require2FAOff, store.Require2FAAdmins, store.Require2FAAll},
		"impact":           impact,
		"inviteSignup":     s.inviteSignupAllowed(),
	})
}

// policyImpact is what a policy would hold back today: the accounts it covers
// that have no second factor, which reach only enrolment until they add one,
// and the API keys those accounts hold, refused meanwhile.
type policyImpact struct {
	Accounts int `json:"accounts"`
	APIKeys  int `json:"apiKeys"`
}

// twoFactorImpact gives each policy's impact, so it can be shown before the
// policy is turned on rather than discovered after.
func (s *Server) twoFactorImpact() (map[string]policyImpact, error) {
	us, err := s.Store.ListUsers()
	if err != nil {
		return nil, err
	}
	keys, err := s.Store.CountAPIKeysByUser()
	if err != nil {
		return nil, err
	}
	out := map[string]policyImpact{}
	for _, p := range []string{store.Require2FAOff, store.Require2FAAdmins, store.Require2FAAll} {
		var imp policyImpact
		for i := range us {
			if u := &us[i]; !u.TOTPEnabled && policyCovers(p, u) {
				imp.Accounts++
				imp.APIKeys += keys[u.ID]
			}
		}
		out[p] = imp
	}
	return out, nil
}

// securitySettingsRequest changes the fields it names. requireTwoFactor was the
// only one, and an empty value still means off, as it always has.
type securitySettingsRequest struct {
	RequireTwoFactor *string `json:"requireTwoFactor"`
	// InviteSignup lets an invitation create the account it is accepted from.
	InviteSignup *bool `json:"inviteSignup"`
}

// handleSetSecuritySettings sets who must hold a second factor, and whether an
// invitation may create an account. Superadmin only.
func (s *Server) handleSetSecuritySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req securitySettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	// Both fields are checked before either is written, so a refused request
	// changes nothing.
	policy := ""
	if req.RequireTwoFactor != nil {
		policy = strings.TrimSpace(*req.RequireTwoFactor)
		switch policy {
		case "", store.Require2FAOff:
			policy = store.Require2FAOff
		case store.Require2FAAdmins, store.Require2FAAll:
		default:
			writeError(w, http.StatusBadRequest, `requireTwoFactor must be "off", "admins" or "all"`)
			return
		}
		// Turning the requirement on locks nobody out: a user without a second
		// factor keeps a session, but it only reaches the enrolment endpoints
		// until they have one (see auth). Refusing the login instead would
		// strand every account at once.
		//
		// Its author is the exception. A superadmin without a second factor who
		// required one was left the enrolment page and nothing else, their API
		// keys refused, including for turning it back off. Whoever requires a
		// second factor holds one first.
		if u := userFrom(r.Context()); policyCovers(policy, u) && !u.TOTPEnabled {
			writeError(w, http.StatusConflict, "enable two-factor authentication on your own account before requiring it")
			return
		}
	}
	if req.InviteSignup != nil {
		v := "on"
		if !*req.InviteSignup {
			v = "off"
		}
		if err := s.Store.SetSetting(store.SettingInviteSignup, v); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.audit(r, 0, "settings.invite-signup", v)
	}
	if policy != "" {
		if err := s.Store.SetSetting(store.SettingRequire2FA, policy); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.audit(r, 0, "settings.require-2fa", policy)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requireTwoFactor": s.requireTwoFactorPolicy(), "inviteSignup": s.inviteSignupAllowed(),
	})
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
	return u != nil && !u.TOTPEnabled && policyCovers(s.requireTwoFactorPolicy(), u)
}

// policyCovers reports whether a two-factor policy applies to u.
func policyCovers(policy string, u *models.User) bool {
	switch policy {
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
