package api

import (
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
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
	if err := s.Store.AddSSHKey(key); err != nil {
		writeError(w, http.StatusInternalServerError, "could not store key")
		return
	}
	s.audit(r, 0, "sshkey.add", key.Fingerprint)
	writeJSON(w, http.StatusCreated, key)
}

func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("kid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	key, err := s.Store.GetSSHKey(uint(id))
	if err != nil || key.UserID != u.ID {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if err := s.Store.DeleteSSHKey(key.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete key")
		return
	}
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
	}
	if srv.SFTP.Enabled {
		if cs, _, err := s.clientsFor(srv); err == nil {
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
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
