package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

func (s *Server) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	keys, err := s.Store.ListSSHKeysForUser(u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list keys")
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"publicKey"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid SSH public key")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = comment
	}
	if name == "" {
		name = "key"
	}
	key := &models.SSHKey{
		UserID:      u.ID,
		Name:        name,
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint: ssh.FingerprintSHA256(pub),
	}
	// Once per account: with two rows for one key, deleting either left the key
	// working through the other, so "revoke" revoked nothing.
	if old, err := s.Store.SSHKeyByFingerprint(u.ID, key.Fingerprint); err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    fmt.Sprintf("this key is already on your account, as %q", old.Name),
			"existing": map[string]any{"id": old.ID, "name": old.Name},
		})
		return
	}
	if err := s.Store.AddSSHKey(key); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			writeError(w, http.StatusConflict, "this key is already on your account")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not store key")
		return
	}
	s.audit(r, 0, "sshkey.add", key.Fingerprint)
	writeJSON(w, http.StatusCreated, key)
}

func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	id, ok := pathID(r, "kid")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	key, err := s.Store.GetSSHKey(id)
	if err != nil || key.UserID != u.ID {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if err := s.Store.DeleteSSHKey(key.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete key")
		return
	}
	s.audit(r, 0, "sshkey.delete", key.Fingerprint)
	w.WriteHeader(http.StatusNoContent)
}

// handleServerSFTP returns a server's SFTP connection details (requires the
// files permission). The port is the assigned NodePort, read live from the
// Service; 0 until Kubernetes assigns it.
func (s *Server) handleServerSFTP(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermFiles)
	if !ok {
		return
	}
	resp := map[string]any{
		"enabled":  srv.SFTP.Enabled,
		"username": userFrom(r.Context()).Username,
		"port":     0,
		"host":     "",
	}
	if srv.SFTP.Enabled {
		cs, _, err := s.clientsFor(srv)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "could not reach the server's cluster")
			return
		}
		svc, err := cs.CoreV1().Services(srv.Namespace).Get(r.Context(), reconciler.SFTPServiceName, metav1.GetOptions{})
		if err == nil {
			for _, p := range svc.Spec.Ports {
				if p.NodePort > 0 {
					resp["port"] = p.NodePort
				}
			}
		} else if !apierrors.IsNotFound(err) {
			writeError(w, http.StatusServiceUnavailable, "could not read SFTP service")
			return
		}
		// Advertise the configured DNS name (or the detected node address) so the
		// connection string matches the game endpoint instead of a placeholder.
		resp["host"] = s.endpointHost(r.Context(), cs, srv.ClusterID)
	}
	writeJSON(w, http.StatusOK, resp)
}
