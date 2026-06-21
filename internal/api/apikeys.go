package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/lolozini/quetzal/internal/models"
)

const apiKeyPrefix = "qk_"

// hashToken returns the hex SHA-256 of a token (what we store for API keys).
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	ks, err := s.Store.ListAPIKeysForUser(u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ks)
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		writeError(w, http.StatusInternalServerError, "rng failed")
		return
	}
	token := apiKeyPrefix + hex.EncodeToString(raw)
	key := &models.APIKey{
		UserID: u.ID,
		Name:   req.Name,
		Prefix: token[:len(apiKeyPrefix)+8],
		Hash:   hashToken(token),
	}
	if err := s.Store.CreateAPIKey(key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "apikey.create", key.Name)
	// The plaintext token is returned exactly once.
	writeJSON(w, http.StatusCreated, map[string]any{"key": key, "token": token})
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	kid, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("kid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid key id")
		return
	}
	key, err := s.Store.GetAPIKey(uint(kid))
	if err != nil || key.UserID != u.ID {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if err := s.Store.DeleteAPIKey(key.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
