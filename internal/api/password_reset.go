package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

const passwordResetTTL = time.Hour

// hashToken (hex SHA-256) is shared with API keys; see apikeys.go.

// publicURL returns the configured external base URL of the panel (empty if
// unset). It is used to build absolute links in email; deliberately not derived
// from request headers so a spoofed Host can't poison reset links.
func (s *Server) publicURL() string {
	v, _ := s.Store.GetSetting(store.SettingPublicURL)
	return strings.TrimSpace(v)
}

// handleForgotPassword starts a self-service password reset. To avoid leaking
// which accounts exist, it always responds 200 regardless of whether the
// identifier matched, and the email (if any) is sent asynchronously so the
// response time doesn't reveal a hit. Requires SMTP + public URL configured.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.AuthIPLimiter.Allow(ip) {
		tooManyRequests(w, s.AuthIPLimiter.RetryAfter(ip))
		return
	}
	var req struct {
		Identifier string `json:"identifier"` // username or email
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	// Uniform response no matter what happens past here.
	defer writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	id := strings.TrimSpace(req.Identifier)
	if id == "" {
		return
	}
	// Per-identifier throttle (separate from the per-IP cap above) to avoid
	// email-bombing a victim.
	if !s.ForgotLimiter.Allow(strings.ToLower(id)) {
		return
	}
	u := s.lookupResetUser(id)
	if u == nil || strings.TrimSpace(u.Email) == "" {
		return
	}
	cfg, _ := s.Store.GetSMTPConfig()
	base := s.publicURL()
	if len(cfg) == 0 || base == "" {
		log.Printf("password reset for %q requested but SMTP/public_url is not configured", u.Username)
		return
	}
	token, err := auth.NewToken()
	if err != nil {
		log.Printf("password reset: token: %v", err)
		return
	}
	// One active token at a time.
	_ = s.Store.DeletePasswordResetsForUser(u.ID)
	if err := s.Store.CreatePasswordReset(&models.PasswordReset{
		UserID:    u.ID,
		TokenHash: hashToken(token),
		ExpiresAt: time.Now().Add(passwordResetTTL),
	}); err != nil {
		log.Printf("password reset: store: %v", err)
		return
	}
	link := strings.TrimRight(base, "/") + "/?reset=" + token
	go s.sendResetEmail(cfg, u, link)
}

// lookupResetUser resolves a reset identifier as a username first, then email.
func (s *Server) lookupResetUser(id string) *models.User {
	if u, err := s.Store.GetUserByUsername(id); err == nil {
		return u
	}
	if u, err := s.Store.GetUserByEmail(id); err == nil {
		return u
	}
	return nil
}

func (s *Server) sendResetEmail(cfg map[string]string, u *models.User, link string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	body := fmt.Sprintf(
		"Hi %s,\n\nWe received a request to reset your Quetzal password. "+
			"Use the link below within %s:\n\n%s\n\n"+
			"If you didn't request this, you can ignore this email.\n",
		u.Username, passwordResetTTL, link,
	)
	if err := s.Mailer(ctx, cfg, []string{u.Email}, "Reset your Quetzal password", body); err != nil {
		log.Printf("password reset: send to user %d: %v", u.ID, err)
	}
}

// handleResetPassword completes a reset: it validates the token, sets the new
// password, then invalidates the token and all the user's sessions so a stolen
// session can't ride along after the password changes.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.AuthIPLimiter.Allow(ip) {
		tooManyRequests(w, s.AuthIPLimiter.RetryAfter(ip))
		return
	}
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, "invalid or expired reset token")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be >=8 chars")
		return
	}
	pr, err := s.Store.GetPasswordResetByHash(hashToken(strings.TrimSpace(req.Token)))
	if err != nil || time.Now().After(pr.ExpiresAt) {
		writeError(w, http.StatusBadRequest, "invalid or expired reset token")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash failed")
		return
	}
	if err := s.Store.UpdateUserPassword(pr.UserID, hash); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.DeletePasswordResetsForUser(pr.UserID)
	_ = s.Store.DeleteSessionsForUser(pr.UserID)
	w.WriteHeader(http.StatusNoContent)
}
