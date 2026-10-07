package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// Share one account budget across sessions, API keys, and sensitive operations.
func (s *Server) allowSensitiveAuth(w http.ResponseWriter, u *models.User) bool {
	key := strconv.FormatUint(uint64(u.ID), 10)
	if !s.SensitiveAuthLimiter.Allow(key) {
		tooManyRequests(w, s.SensitiveAuthLimiter.RetryAfter(key))
		return false
	}
	return true
}

func writeCredentialError(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrBusy) {
		tooManyRequests(w, 1)
		return
	}
	if errors.Is(err, store.ErrCredentialsChanged) {
		writeError(w, http.StatusUnauthorized, "credentials changed; authenticate again")
		return
	}
	writeError(w, http.StatusInternalServerError, "credential operation failed")
}
