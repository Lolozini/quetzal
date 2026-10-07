package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/mailtmpl"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/notify"
	"github.com/lolozini/quetzal/internal/store"
)

// handleGetEmailSettings returns the system SMTP settings (admin only). The
// password is never returned, only whether one is set.
func (s *Server) handleGetEmailSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermSettings) {
		return
	}
	cfg, err := s.Store.GetSMTPConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read email settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":  len(cfg) > 0,
		"host":        cfg["host"],
		"port":        cfg["port"],
		"username":    cfg["username"],
		"from":        cfg["from"],
		"replyTo":     cfg["replyTo"],
		"tls":         cfg["tls"],
		"hasPassword": cfg["password"] != "",
		"publicUrl":   s.publicURL(),
	})
}

type emailSettingsRequest struct {
	Host      string `json:"host"`
	Port      string `json:"port"`
	Username  string `json:"username"`
	Password  string `json:"password"` // empty keeps the stored one
	From      string `json:"from"`
	ReplyTo   string `json:"replyTo"`
	TLS       string `json:"tls"` // "starttls" | "tls" | "none"
	PublicURL string `json:"publicUrl"`
}

// handleSetEmailSettings updates the SMTP settings + public URL. An empty host
// clears the SMTP config (disables system email).
//
// Superadmin, not the settings permission that guards the rest of this file:
// whoever chooses the relay reads every mail the panel sends, and one of those
// is a password reset link. A scoped admin who could repoint it -- or repoint
// only the public URL the link is built from -- could ask for the superadmin's
// reset and take the account. Reading these settings stays delegated; they come
// back with the password redacted.
func (s *Server) handleSetEmailSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req emailSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.Store.SetSetting(store.SettingPublicURL, strings.TrimSpace(req.PublicURL)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if strings.TrimSpace(req.Host) == "" {
		if err := s.Store.SetSMTPConfig(nil); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.audit(r, 0, "email.settings.clear", "")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Refused here rather than by the relay on every send, password resets
	// included, which nobody sees until someone needs one.
	if strings.TrimSpace(req.From) == "" {
		writeError(w, http.StatusBadRequest, "from is required: the email address the panel's mail comes from")
		return
	}
	if _, err := notify.ParseFrom(req.From); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// An address a reader can write back to. Mail from a noreply@ address
	// nobody answers is read as less legitimate, by people and by filters.
	if rt := strings.TrimSpace(req.ReplyTo); rt != "" {
		if _, err := notify.ParseFrom(rt); err != nil {
			writeError(w, http.StatusBadRequest, "reply-to: "+err.Error())
			return
		}
	}
	// Preserve the stored password when the form leaves it blank.
	password := req.Password
	if password == "" {
		if existing, _ := s.Store.GetSMTPConfig(); existing != nil {
			password = existing["password"]
		}
	}
	cfg := map[string]string{
		"host":     strings.TrimSpace(req.Host),
		"port":     strings.TrimSpace(req.Port),
		"username": strings.TrimSpace(req.Username),
		"password": password,
		"from":     strings.TrimSpace(req.From),
		"replyTo":  strings.TrimSpace(req.ReplyTo),
		"tls":      strings.TrimSpace(req.TLS),
	}
	if err := s.Store.SetSMTPConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "email.settings.update", cfg["host"])
	w.WriteHeader(http.StatusNoContent)
}

// handleTestEmail sends a test message with the stored settings (admin only).
func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermSettings) {
		return
	}
	// A test mail goes where the caller says, from the operator's domain: one
	// a settings administrator could send without end.
	key := strconv.FormatUint(uint64(userFrom(r.Context()).ID), 10)
	if !s.TestMailLimiter.Allow(key) {
		tooManyRequests(w, s.TestMailLimiter.RetryAfter(key))
		return
	}
	var req struct {
		To string `json:"to"`
	}
	_ = decodeJSON(r, &req)
	to := strings.TrimSpace(req.To)
	if to == "" {
		to = strings.TrimSpace(userFrom(r.Context()).Email)
	}
	if to == "" {
		writeError(w, http.StatusBadRequest, "no recipient (set your account email or pass one)")
		return
	}
	cfg, _ := s.Store.GetSMTPConfig()
	if len(cfg) == 0 {
		writeError(w, http.StatusBadRequest, "email is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	m, err := mailtmpl.Test(s.publicURL())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Mailer(ctx, cfg, []string{to}, m); err != nil {
		writeError(w, http.StatusBadGateway, "send failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
