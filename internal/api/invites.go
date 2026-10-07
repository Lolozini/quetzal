package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/mailtmpl"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/notify"
	"github.com/lolozini/quetzal/internal/store"
)

const (
	// inviteTTL is how long an invitation link stays good.
	inviteTTL = 7 * 24 * time.Hour
	// maxOpenInvites bounds a server's open invitations: each one is a mail sent
	// to an address of the inviter's choosing.
	maxOpenInvites = 50
)

// inviteSignupAllowed reads whether an invitation may create the account it is
// accepted from. On unless an administrator turned it off.
func (s *Server) inviteSignupAllowed() bool {
	v, _ := s.Store.GetSetting(store.SettingInviteSignup)
	return strings.TrimSpace(v) != "off"
}

// inviteEmail checks an address to invite: one bare address, which fits the
// account it may become.
func inviteEmail(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	a, err := mail.ParseAddress(raw)
	if err != nil || a.Name != "" || a.Address != raw || len(raw) > 190 || !looksLikeEmail(raw) {
		return "", false
	}
	return strings.ToLower(raw), true
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireOwnerOrAdmin(w, r)
	if !ok {
		return
	}
	invs, err := s.Store.ListServerInvites(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, invs)
}

// handleCreateInvite mails an invitation to a server. The address is not looked
// up among accounts: whoever reads that mailbox accepts it, from the account
// they sign in to or one they create, so an account that merely lists the
// address in its profile gains nothing. Inviting the same address again sends a
// new link and retires the old one.
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireOwnerOrAdmin(w, r)
	if !ok {
		return
	}
	var req struct {
		Email       string   `json:"email"`
		Permissions []string `json:"permissions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	email, ok := inviteEmail(req.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid email")
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
	cfg, _ := s.Store.GetSMTPConfig()
	base := s.publicURL()
	if len(cfg) == 0 || base == "" {
		writeError(w, http.StatusConflict, "this panel cannot send email yet: an administrator sets up email and the panel's public address first")
		return
	}
	if n, err := s.Store.CountServerInvites(srv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if n >= maxOpenInvites {
		writeError(w, http.StatusConflict, fmt.Sprintf("this server already has %d open invitations; withdraw some first", n))
		return
	}
	u := userFrom(r.Context())
	key := strconv.FormatUint(uint64(u.ID), 10)
	if !s.InviteLimiter.Allow(key) {
		tooManyRequests(w, s.InviteLimiter.RetryAfter(key))
		return
	}
	token, err := auth.NewToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "token failed")
		return
	}
	inv := &models.ServerInvite{
		ServerID: srv.ID, Email: email, Permissions: req.Permissions, InvitedBy: u.ID,
		TokenHash: hashToken(token), ExpiresAt: time.Now().Add(inviteTTL),
	}
	if err := s.Store.SetServerInvite(inv); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Sent before answering, unlike a password reset's: the inviter knows the
	// address, so there is nothing to hide, and they should hear that the mail
	// did not leave. In the fragment, like a reset link, so that the token
	// stays out of access logs.
	link := strings.TrimRight(base, "/") + "/#invite=" + token
	m, err := mailtmpl.Invitation(u.Username, srv.DisplayName, inv.Permissions, link, inv.ExpiresAt, s.inviteSignupAllowed(), base)
	if err != nil {
		_ = s.Store.DeleteServerInvite(inv.ID)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.Mailer(ctx, cfg, []string{email}, m); err != nil {
		_ = s.Store.DeleteServerInvite(inv.ID)
		log.Printf("invitation to server %d: send: %v", srv.ID, err)
		// An address the mail server refuses is the address's fault, not the
		// settings': saying to check them sent an administrator after nothing.
		var rcpt *notify.RecipientError
		if errors.As(err, &rcpt) {
			writeError(w, http.StatusBadRequest, "the mail server refused this address: "+rcpt.Err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "the invitation could not be sent; an administrator can check the email settings")
		return
	}
	inv.InvitedByName = u.Username
	s.audit(r, srv.ID, "access.invite", email+": "+strings.Join(req.Permissions, ","))
	writeJSON(w, http.StatusCreated, inv)
}

func (s *Server) handleDeleteInvite(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireOwnerOrAdmin(w, r)
	if !ok {
		return
	}
	iid, ok := pathID(r, "iid")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid invitation id")
		return
	}
	inv, err := s.Store.GetServerInvite(srv.ID, iid)
	if err != nil {
		writeError(w, http.StatusNotFound, "invitation not found")
		return
	}
	if err := s.Store.DeleteServerInvite(inv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "access.invite-revoke", inv.Email)
	w.WriteHeader(http.StatusNoContent)
}

// inviteFromToken resolves the token a request carries to its open invitation
// and server, answering for it when there is none.
func (s *Server) inviteFromToken(w http.ResponseWriter, token string) (*models.ServerInvite, *models.Server, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		writeError(w, http.StatusNotFound, "this invitation is no longer valid")
		return nil, nil, false
	}
	inv, err := s.Store.GetServerInviteByHash(hashToken(token))
	if err != nil {
		writeError(w, http.StatusNotFound, "this invitation is no longer valid")
		return nil, nil, false
	}
	srv, err := s.Store.GetServer(inv.ServerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "this invitation is no longer valid")
		return nil, nil, false
	}
	return inv, srv, true
}

type inviteTokenRequest struct {
	Token string `json:"token"`
}

// handleInspectInvite tells the holder of a link what it offers, before they
// sign in or create an account. Public: the token is the credential.
func (s *Server) handleInspectInvite(w http.ResponseWriter, r *http.Request) {
	ip := s.authAddress(r)
	if !s.AuthIPLimiter.Allow(ip) {
		tooManyRequests(w, s.AuthIPLimiter.RetryAfter(ip))
		return
	}
	var req inviteTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	inv, srv, ok := s.inviteFromToken(w, req.Token)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":      srv.DisplayName,
		"invitedBy":   inv.InvitedByName,
		"email":       inv.Email,
		"permissions": inv.Permissions,
		"expiresAt":   inv.ExpiresAt,
		"signup":      s.inviteSignupAllowed(),
	})
}

// handleAcceptInvite accepts an invitation with the account signed in.
func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req inviteTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	inv, srv, ok := s.inviteFromToken(w, req.Token)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if srv.OwnerID == u.ID {
		writeError(w, http.StatusBadRequest, "you already own this server")
		return
	}
	if err := s.Store.AcceptServerInvite(inv, u.ID); err != nil {
		writeError(w, http.StatusNotFound, "this invitation is no longer valid")
		return
	}
	s.audit(r, srv.ID, "access.grant", u.Username+": "+strings.Join(inv.Permissions, ",")+" (invitation to "+inv.Email+")")
	writeJSON(w, http.StatusOK, map[string]uint{"serverId": srv.ID})
}

// handleRegisterFromInvite creates an account from an invitation and signs it
// in. The account is the one the panel gives anyone new: it may own no server
// until an administrator says otherwise, and its email is the invited address,
// which the link has just shown to be theirs.
func (s *Server) handleRegisterFromInvite(w http.ResponseWriter, r *http.Request) {
	ip := s.authAddress(r)
	if !s.AuthIPLimiter.Allow(ip) {
		tooManyRequests(w, s.AuthIPLimiter.RetryAfter(ip))
		return
	}
	var req struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	inv, srv, ok := s.inviteFromToken(w, req.Token)
	if !ok {
		return
	}
	if !s.inviteSignupAllowed() {
		writeError(w, http.StatusForbidden, "this panel does not create accounts from invitations; sign in to an existing account to accept")
		return
	}
	username := strings.TrimSpace(req.Username)
	if err := models.ValidUsername(username); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "a password is at least 8 characters")
		return
	}
	if _, err := s.Store.GetUserByUsername(username); err == nil {
		writeError(w, http.StatusConflict, "username already taken")
		return
	}
	// The address the invitation went to is the new account's: one that an
	// account has already is that account's, which can accept by signing in.
	if taken, err := s.Store.EmailTaken(inv.Email, 0); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if taken {
		writeError(w, http.StatusConflict, "an account already uses this email address: sign in to it to accept the invitation")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	u := &models.User{
		Username: username, PasswordHash: hash, Email: inv.Email,
		MaxServers: 0, MaxMemoryMB: models.QuotaUnlimited, MaxCPUMilli: models.QuotaUnlimited,

		// The invitation went to this address, and its link was opened.
		EmailVerified: true,
	}
	if err := s.Store.RegisterFromInvite(inv, u); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "this invitation is no longer valid")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// There was no session: the entries go under the account just made.
	rr := r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
	s.audit(rr, 0, "user.create", u.Username+" (invitation to "+inv.Email+")")
	s.audit(rr, srv.ID, "access.grant", u.Username+": "+strings.Join(inv.Permissions, ",")+" (invitation to "+inv.Email+")")
	if err := s.startSession(w, u); err != nil {
		writeCredentialError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": u, "serverId": srv.ID})
}
