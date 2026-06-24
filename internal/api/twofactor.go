package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/totp"
)

// twoFactorIssuer labels the account in authenticator apps.
const twoFactorIssuer = "Quetzal"

// verifyTwoFactor reports whether code is a valid current TOTP code or an
// unused recovery code for the user. A matching recovery code is consumed.
func (s *Server) verifyTwoFactor(u *models.User, code string) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}
	if secret, err := s.Store.UserTOTPSecret(u); err == nil && secret != "" && totp.Validate(secret, code) {
		return true
	}
	ok, _ := s.Store.ConsumeRecoveryCode(u.ID, code)
	return ok
}

// handle2FASetup starts enrollment: it generates and stores a pending secret and
// returns it (with an otpauth URI) for the user to add to their authenticator.
// Enrollment is not active until confirmed via handle2FAEnable.
func (s *Server) handle2FASetup(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u.TOTPEnabled {
		writeError(w, http.StatusConflict, "two-factor authentication is already enabled")
		return
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate secret")
		return
	}
	if err := s.Store.SetUserTOTPSecret(u.ID, secret); err != nil {
		writeError(w, http.StatusInternalServerError, "could not store secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": secret,
		"uri":    totp.URI(secret, twoFactorIssuer, u.Username),
	})
}

// handle2FAEnable confirms enrollment with a valid code, activates 2FA, and
// returns freshly generated single-use recovery codes (shown only once).
func (s *Server) handle2FAEnable(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if u.TOTPEnabled {
		writeError(w, http.StatusConflict, "two-factor authentication is already enabled")
		return
	}
	secret, err := s.Store.UserTOTPSecret(u)
	if err != nil || secret == "" {
		writeError(w, http.StatusBadRequest, "start enrollment first")
		return
	}
	if !totp.Validate(secret, req.Code) {
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}
	plain, hashes, err := totp.NewRecoveryCodes(10)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate recovery codes")
		return
	}
	if err := s.Store.EnableUserTOTP(u.ID, hashes); err != nil {
		writeError(w, http.StatusInternalServerError, "could not enable two-factor")
		return
	}
	s.audit(r, 0, "2fa.enable", u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": plain})
}

// handle2FADisable turns off 2FA for the current user after re-verifying a
// current TOTP or recovery code (so a hijacked live session can't silently
// remove it).
func (s *Server) handle2FADisable(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !u.TOTPEnabled {
		writeError(w, http.StatusBadRequest, "two-factor authentication is not enabled")
		return
	}
	if !s.verifyTwoFactor(u, req.Code) {
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}
	if err := s.Store.DisableUserTOTP(u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not disable two-factor")
		return
	}
	s.audit(r, 0, "2fa.disable", u.Username)
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminDisable2FA lets an admin clear another user's 2FA, the recovery
// path when a user loses their authenticator and all recovery codes.
func (s *Server) handleAdminDisable2FA(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermUsers) {
		return
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("uid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	target, err := s.Store.GetUser(uint(uid))
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err := s.Store.DisableUserTOTP(target.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not reset two-factor")
		return
	}
	s.audit(r, 0, "2fa.admin-reset", target.Username)
	w.WriteHeader(http.StatusNoContent)
}
