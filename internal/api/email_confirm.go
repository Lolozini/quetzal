package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/mailtmpl"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// emailConfirmationTTL is how long a confirmation link works: a day, as the
// mail may be read the next morning.
const emailConfirmationTTL = 24 * time.Hour

// confirmationMail returns the SMTP settings and public URL a confirmation
// link needs, or nil when the panel cannot send one.
func (s *Server) confirmationMail() (map[string]string, string) {
	cfg, _ := s.Store.GetSMTPConfig()
	base := s.publicURL()
	if len(cfg) == 0 || base == "" {
		return nil, ""
	}
	return cfg, base
}

// sendEmailConfirmation mails u a link that confirms address, and records it.
// The mail is sent before the answer, unlike a reset's: the person asking is
// signed in and is told when it could not go. It writes the error itself.
func (s *Server) sendEmailConfirmation(w http.ResponseWriter, r *http.Request, cfg map[string]string, base string, u *models.User, address string) bool {
	key := strconv.FormatUint(uint64(u.ID), 10)
	if !s.ConfirmMailLimiter.Allow(key) {
		tooManyRequests(w, s.ConfirmMailLimiter.RetryAfter(key))
		return false
	}
	token, err := auth.NewToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	// In the fragment, as a reset token is: never sent to a server or its logs.
	link := strings.TrimRight(base, "/") + "/#confirm-email=" + token
	m, err := mailtmpl.EmailConfirmation(u.Username, address, link, emailConfirmationTTL, s.publicURL())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.Mailer(ctx, cfg, []string{address}, m); err != nil {
		writeError(w, http.StatusBadGateway, "could not send the confirmation mail: "+err.Error())
		return false
	}
	if err := s.Store.StartEmailConfirmation(&models.EmailConfirmation{
		UserID: u.ID, Email: address, TokenHash: hashToken(token), ExpiresAt: time.Now().Add(emailConfirmationTTL),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	return true
}

// handleResendEmailConfirmation mails a new link: to the address waiting for
// confirmation, or to the account's own when it was never confirmed (set by an
// administrator, or before confirmation existed).
func (s *Server) handleResendEmailConfirmation(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	address := u.PendingEmail
	if address == "" && !u.EmailVerified {
		address = u.Email
	}
	if address == "" {
		writeError(w, http.StatusConflict, "there is no address to confirm")
		return
	}
	cfg, base := s.confirmationMail()
	if cfg == nil {
		writeError(w, http.StatusConflict, "the panel cannot send mail: an administrator has to set up email and the panel's public URL")
		return
	}
	if !s.sendEmailConfirmation(w, r, cfg, base, u, address) {
		return
	}
	updated, _ := s.Store.GetUser(u.ID)
	writeJSON(w, http.StatusOK, updated)
}

// handleCancelPendingEmail drops the address waiting for confirmation.
func (s *Server) handleCancelPendingEmail(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := s.Store.CancelPendingEmail(u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, _ := s.Store.GetUser(u.ID)
	writeJSON(w, http.StatusOK, updated)
}

// handleConfirmEmail takes the token of a confirmation link. It needs no
// session: the link may be opened on a phone the account never signed in on.
func (s *Server) handleConfirmEmail(w http.ResponseWriter, r *http.Request) {
	ip := s.authAddress(r)
	if !s.AuthIPLimiter.Allow(ip) {
		tooManyRequests(w, s.AuthIPLimiter.RetryAfter(ip))
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeError(w, http.StatusBadRequest, store.ErrConfirmationInvalid.Error())
		return
	}
	u, err := s.Store.ConfirmEmail(hashToken(token))
	switch {
	case errors.Is(err, store.ErrConfirmationInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, "another account took this email address meanwhile")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r.WithContext(context.WithValue(r.Context(), userCtxKey, u)), 0, "user.email", "confirmed "+u.Email)
	writeJSON(w, http.StatusOK, map[string]string{"email": u.Email})
}
